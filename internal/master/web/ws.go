package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "craftstack/gen/proto/craftstack"
)

// WSClient represents a connected WebSocket client.
type WSClient struct {
	conn       *websocket.Conn
	instanceID string
	send       chan []byte
}

// WSHub manages WebSocket connections for log streaming.
type WSHub struct {
	log        *slog.Logger
	mu         sync.RWMutex
	clients    map[*WSClient]bool
	register   chan *WSClient
	unregister chan *WSClient
	broadcast  chan WSMessage
}

// WSMessage is a message to broadcast to specific instance subscribers.
type WSMessage struct {
	InstanceID string `json:"instance_id"`
	Data       string `json:"data"`
}

// NewWSHub creates a new WebSocket hub.
func NewWSHub(log *slog.Logger) *WSHub {
	return &WSHub{
		log:        log,
		clients:    make(map[*WSClient]bool),
		register:   make(chan *WSClient),
		unregister: make(chan *WSClient),
		broadcast:  make(chan WSMessage, 1000),
	}
}

// Run starts the hub's event loop.
func (h *WSHub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.log.Debug("ws client connected", "instance", client.instanceID)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			h.log.Debug("ws client disconnected", "instance", client.instanceID)

		case msg := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				if client.instanceID == msg.InstanceID || client.instanceID == "*" {
					select {
					case client.send <- []byte(msg.Data):
					default:
						// Buffer full, skip
					}
				}
			}
			h.mu.RUnlock()
		}
	}
}

// Broadcast sends a message to all clients watching a specific instance.
func (h *WSHub) Broadcast(instanceID, data string) {
	h.broadcast <- WSMessage{
		InstanceID: instanceID,
		Data:       data,
	}
}

// handleWSLogs handles WebSocket connections for log streaming.
func (s *Server) handleWSLogs(c echo.Context) error {
	instanceID := c.Param("instanceId")

	wsServer := websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			// Origin check skip — mobile browser  compatibility
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()

			client := &WSClient{
				conn:       ws,
				instanceID: instanceID,
				send:       make(chan []byte, 256),
			}

			s.hub.register <- client
			defer func() {
				s.hub.unregister <- client
			}()

			// Write pump
			go func() {
				for msg := range client.send {
					if err := websocket.Message.Send(ws, string(msg)); err != nil {
						return
					}
				}
			}()

			// audit log sent (ring buffer save recent log)
			if history := s.connector.GetLogHistory(instanceID); len(history) > 0 {
				for _, line := range history {
					select {
					case client.send <- []byte(line):
					default:
						// Buffer full, skip remaining history
						break
					}
				}
			}

			// Read pump (keep connection alive, receive commands)
			for {
				var msg string
				if err := websocket.Message.Receive(ws, &msg); err != nil {
					break
				}

				cookie, cookieErr := c.Cookie(sessionCookieName)
				if cookieErr != nil || s.sessions.Get(cookie.Value) == nil {
					return
				}
				if role, _ := c.Get("user_role").(string); role == "viewer" {
					continue
				}
				if edit, restricted := c.Get("instance_can_edit").(bool); restricted && !edit {
					continue
				}
				s.log.Debug("ws command received", "instance", instanceID, "command", msg)

				// instance agent as console command forward (inmemory stateful query)
				go func(cmd string) {
					// inmemory from instance → agent mapping query
					agentID, ok := s.connector.GetInstanceOwner(instanceID)
					if !ok {
						errMsg := "[system] instance not yet registered. please wait for agent heartbeat."
						s.log.Warn("instance owner agent none", "instance", instanceID)
						select {
						case client.send <- []byte(errMsg):
						default:
						}
						return
					}

					agentAddr, ok := s.connector.GetAgentAddress(agentID)
					if !ok {
						errMsg := "[system] the agent offline."
						s.log.Warn("agent offline", "agent_id", agentID)
						select {
						case client.send <- []byte(errMsg):
						default:
						}
						return
					}

					conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err != nil {
						errMsg := "[system] agent connection failed"
						s.log.Error("agent connection failed", "error", err)
						select {
						case client.send <- []byte(errMsg):
						default:
						}
						return
					}
					defer conn.Close()

					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					metricsClient := pb.NewMetricsServiceClient(conn)
					resp, err := metricsClient.SendCommand(ctx, &pb.ConsoleCommandRequest{
						AgentId:    agentID,
						InstanceId: instanceID,
						Command:    cmd,
					})
					if err != nil {
						errMsg := fmt.Sprintf("[system] command forward failed: %v", err)
						s.log.Error("console command forward failed", "error", err)
						select {
						case client.send <- []byte(errMsg):
						default:
						}
						return
					}
					if !resp.Success {
						errMsg := fmt.Sprintf("[system] command failed: %s", resp.Error)
						s.log.Warn("console command failed", "error", resp.Error)
						select {
						case client.send <- []byte(errMsg):
						default:
						}
					}
				}(msg)
			}
		},
	}
	wsServer.ServeHTTP(c.Response(), c.Request())

	return nil
}
