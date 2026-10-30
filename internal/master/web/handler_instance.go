package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "craftstack/gen/proto/craftstack"
	"craftstack/internal/master/store"
)

func (s *Server) handleInstances(c echo.Context) error {
	instances, err := s.db.ListInstances("")
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	instances = s.visibleInstances(c, instances)
	s.overlayInstanceStatus(instances)

	// online agent list (create instance modal)
	nodes, _ := s.db.ListNodes()
	s.overlayNodeStatus(nodes)

	data := map[string]interface{}{
		"Title":     "instance management",
		"Instances": instances,
		"Nodes":     nodes,
	}
	s.enrichInstanceNetworkData(data, instances)
	return renderPage(c, "instances", data)
}

func (s *Server) handleInstanceDetail(c echo.Context) error {
	id := c.Param("id")
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "instance not found")
	}
	// node offlineif instance offline
	if !s.connector.IsAgentOnline(inst.NodeID) {
		inst.Status = "offline"
	}
	backups, err := s.db.ListBackups(id, 10)
	if err != nil {
		s.log.Error("backup list query failed", "error", err)
	}

	// node WireGuard info query
	var nodeWGAddress, nodeDockerSubnet string
	if node, err := s.db.GetNode(inst.NodeID); err == nil {
		nodeWGAddress = node.WGAddress
		nodeDockerSubnet = node.DockerSubnet
	}

	// instance connect network info query
	instNetworks, _ := s.db.ListInstanceNetworks(id)

	data := map[string]interface{}{
		"Title":            fmt.Sprintf("instance: %s", inst.Name),
		"Instance":         inst,
		"Backups":          backups,
		"NodeWGAddress":    nodeWGAddress,
		"NodeDockerSubnet": nodeDockerSubnet,
		"InstanceNetworks": instNetworks,
	}
	return renderPage(c, "instance_detail", data)
}

func (s *Server) handleConsole(c echo.Context) error {
	id := c.Param("id")
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "instance not found")
	}
	data := map[string]interface{}{
		"Title":    fmt.Sprintf("console: %s", inst.Name),
		"Instance": inst,
	}
	return renderPage(c, "console", data)
}

func (s *Server) apiListInstances(c echo.Context) error {
	nodeID := c.QueryParam("node_id")
	instances, err := s.db.ListInstances(nodeID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if instances == nil {
		instances = []*store.Instance{}
	}
	instances = s.visibleInstances(c, instances)
	s.overlayInstanceStatus(instances)
	return c.JSON(http.StatusOK, instances)
}

// apiGetInstance returns a single instance by ID.
func (s *Server) apiGetInstance(c echo.Context) error {
	id := c.Param("id")
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "instance not found",
		})
	}
	s.overlayInstanceStatus([]*store.Instance{inst})
	return c.JSON(http.StatusOK, inst)
}

func (s *Server) apiControlInstance(c echo.Context) error {
	id := c.Param("id")
	var req struct {
		Action string `json:"action"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request")
	}

	s.log.Info("control command", "instance_id", id, "action", req.Action)

	actionNames := map[string]string{
		"start":   "start",
		"stop":    "stop",
		"restart": "restart",
		"kill":    "force shutdown",
	}
	actionName := actionNames[req.Action]
	if actionName == "" {
		actionName = req.Action
	}

	// proto action convert
	var pbAction pb.InstanceAction
	switch req.Action {
	case "start":
		pbAction = pb.InstanceAction_INSTANCE_ACTION_START
	case "stop":
		pbAction = pb.InstanceAction_INSTANCE_ACTION_STOP
	case "restart":
		pbAction = pb.InstanceAction_INSTANCE_ACTION_RESTART
	case "kill":
		pbAction = pb.InstanceAction_INSTANCE_ACTION_KILL
	default:
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("unknown command: %s", req.Action),
		})
	}

	// inmemory from instance → agent mapping query (stateful)
	agentID, found := s.connector.GetInstanceOwner(id)
	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "instance not yet registered. please wait for agent heartbeat.",
		})
	}

	// agent address query
	agentAddr, ok := s.connector.GetAgentAddress(agentID)
	if !ok {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status":  "error",
			"message": "the agent offline",
		})
	}

	// agent AgentService.ControlInstance call
	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("agent connection failed: %v", err),
		})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	agentClient := pb.NewAgentServiceClient(conn)
	resp, err := agentClient.ControlInstance(ctx, &pb.ControlInstanceRequest{
		AgentId:    agentID,
		InstanceId: id,
		Action:     pbAction,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("command forward failed: %v", err),
		})
	}

	if !resp.Success {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": resp.Message,
		})
	}

	// audit log: instance control
	instForAudit, _ := s.db.GetInstance(id)
	instName := id
	if instForAudit != nil {
		instName = instForAudit.Name
	}
	s.audit(c, req.Action, "instance", id, instName, "", "", "",
		fmt.Sprintf("instance %s: %s", instName, actionName))

	return c.JSON(http.StatusOK, map[string]string{
		"status":  "accepted",
		"action":  req.Action,
		"message": fmt.Sprintf("instance %s '%s' command senddone", id, actionName),
	})
}

// apiDeleteInstance handles instance deletion.
// 1. agent DeleteInstance RPC call (Docker remove container)
// 2. DB from delete instance
// 3. connect network info delete
func (s *Server) apiDeleteInstance(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		RemoveData bool `json:"remove_data"`
	}
	// bind failed default(false) as proceed
	c.Bind(&req)

	s.log.Info("delete instance request", "instance_id", id, "remove_data", req.RemoveData)

	// instance query
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "instance not found",
		})
	}

	// the agent onlineif connect network release + Docker remove container request
	agentAddr, ok := s.connector.GetAgentAddress(inst.NodeID)
	if ok {
		conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			defer conn.Close()

			ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
			defer cancel()

			agentClient := pb.NewAgentServiceClient(conn)

			// first connect network release (remove container before)
			containerName := fmt.Sprintf("craftstack-%s", inst.Name)
			instNetworksForDisconnect, _ := s.db.ListInstanceNetworks(id)
			for _, in := range instNetworksForDisconnect {
				if net, err := s.db.GetNetwork(in.NetworkID); err == nil {
					agentClient.DisconnectNetwork(ctx, &pb.DisconnectNetworkRequest{
						NetworkName:   net.Name,
						ContainerName: containerName,
					})
				}
			}

			// remove container
			resp, err := agentClient.DeleteInstance(ctx, &pb.DeleteInstanceRequest{
				InstanceId: id,
				RemoveData: req.RemoveData,
			})
			if err != nil {
				s.log.Warn("agent delete instance failed (DB fromonly delete)", "error", err)
			} else if !resp.Success {
				s.log.Warn("agent delete instance failed", "message", resp.Message)
			}
		}
	} else {
		s.log.Info("agent offline, DB fromonly delete", "node_id", inst.NodeID)
	}

	// mesh DNS record delete
	if s.mesh != nil {
		s.mesh.UnregisterInstanceDNS(id)
	}

	// connect network info DB from delete
	instNetworks, err := s.db.ListInstanceNetworks(id)
	if err == nil {
		for _, in := range instNetworks {
			s.db.RemoveInstanceFromNetwork(id, in.NetworkID)
		}
	}

	// DB from delete instance
	if err := s.db.DeleteInstance(id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("DB delete failed: %v", err),
		})
	}

	// audit log: delete instance
	s.audit(c, "delete", "instance", id, inst.Name, "", "", "",
		fmt.Sprintf("delete instance: %s (data delete: %v)", inst.Name, req.RemoveData))

	return c.JSON(http.StatusOK, map[string]string{
		"status":  "success",
		"message": "the instance deleted",
	})
}

// apiUpdateInstance handles instance configuration update.
func (s *Server) apiUpdateInstance(c echo.Context) error {
	id := c.Param("id")

	// existing instance query
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "instance not found",
		})
	}

	var req struct {
		Name           *string `json:"name"`
		Port           *int    `json:"port"`
		MemoryMin      *string `json:"memory_min"`
		MemoryMax      *string `json:"memory_max"`
		AutoStart      *bool   `json:"auto_start"`
		AutoRestart    *bool   `json:"auto_restart"`
		StopCommand    *string `json:"stop_command"`
		JVMArgs        *string `json:"jvm_args"`
		ServiceVersion *string `json:"service_version"`

		// MySQL
		MySQLRootPassword *string `json:"mysql_root_password"`
		MySQLExtraArgs    *string `json:"mysql_extra_args"`

		// PostgreSQL
		PGPassword  *string `json:"pg_password"`
		PGExtraArgs *string `json:"pg_extra_args"`

		// MongoDB
		MongoAdminUser     *string `json:"mongo_admin_user"`
		MongoAdminPassword *string `json:"mongo_admin_password"`
		MongoExtraArgs     *string `json:"mongo_extra_args"`

		// Redis
		RedisPassword  *string `json:"redis_password"`
		RedisExtraArgs *string `json:"redis_extra_args"`

		// Kafka
		KafkaBrokerID  *int    `json:"kafka_broker_id"`
		KafkaExtraArgs *string `json:"kafka_extra_args"`

		// backup schedule
		BackupEnabled  *bool   `json:"backup_enabled"`
		BackupCron     *string `json:"backup_cron"`
		BackupMaxCount *int    `json:"backup_max_count"`

		// Java version + Docker customization
		JavaVersion      *string `json:"java_version"`
		CustomDockerfile *string `json:"custom_dockerfile"`
		CustomCompose    *string `json:"custom_compose"`

		// Docker resource limit
		DockerMemory *string `json:"docker_memory"`
		DockerCPUs   *string `json:"docker_cpus"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "invalid request",
		})
	}

	// change before value save (audit log)
	oldInst := *inst

	// change available fieldonly update (nilif existingvalue keep)
	// Note: Port is immutable once set (req.Port ignore)
	if req.Name != nil {
		inst.Name = *req.Name
	}
	if req.MemoryMin != nil {
		inst.MemoryMin = *req.MemoryMin
	}
	if req.MemoryMax != nil {
		inst.MemoryMax = *req.MemoryMax
	}
	if req.AutoStart != nil {
		inst.AutoStart = *req.AutoStart
	}
	if req.AutoRestart != nil {
		inst.AutoRestart = *req.AutoRestart
	}
	if req.StopCommand != nil {
		inst.StopCommand = *req.StopCommand
	}
	if req.JVMArgs != nil {
		inst.JVMArgs = *req.JVMArgs
	}
	if req.ServiceVersion != nil {
		inst.ServiceVersion = *req.ServiceVersion
	}

	// typeper field
	if req.MySQLRootPassword != nil {
		inst.MySQLRootPassword = *req.MySQLRootPassword
	}
	if req.MySQLExtraArgs != nil {
		inst.MySQLExtraArgs = *req.MySQLExtraArgs
	}
	if req.PGPassword != nil {
		inst.PGPassword = *req.PGPassword
	}
	if req.PGExtraArgs != nil {
		inst.PGExtraArgs = *req.PGExtraArgs
	}
	if req.MongoAdminUser != nil {
		inst.MongoAdminUser = *req.MongoAdminUser
	}
	if req.MongoAdminPassword != nil {
		inst.MongoAdminPassword = *req.MongoAdminPassword
	}
	if req.MongoExtraArgs != nil {
		inst.MongoExtraArgs = *req.MongoExtraArgs
	}
	if req.RedisPassword != nil {
		inst.RedisPassword = *req.RedisPassword
	}
	if req.RedisExtraArgs != nil {
		inst.RedisExtraArgs = *req.RedisExtraArgs
	}
	if req.KafkaBrokerID != nil {
		inst.KafkaBrokerID = *req.KafkaBrokerID
	}
	if req.KafkaExtraArgs != nil {
		inst.KafkaExtraArgs = *req.KafkaExtraArgs
	}

	// backup schedule field
	if req.BackupEnabled != nil {
		inst.BackupEnabled = *req.BackupEnabled
	}
	if req.BackupCron != nil {
		cron := strings.TrimSpace(*req.BackupCron)
		if cron != "" {
			fields := strings.Fields(cron)
			if len(fields) != 5 {
				return c.JSON(http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": "cron expression 5 field(min when day month day) is required",
				})
			}
		}
		inst.BackupCron = cron
	}
	if req.BackupMaxCount != nil {
		if *req.BackupMaxCount < 1 {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"status":  "error",
				"message": "max backup retention count 1 must be at least ",
			})
		}
		inst.BackupMaxCount = *req.BackupMaxCount
	}

	// Java version + Docker customization
	if req.JavaVersion != nil {
		inst.JavaVersion = *req.JavaVersion
	}
	if req.CustomDockerfile != nil {
		inst.CustomDockerfile = *req.CustomDockerfile
	}
	if req.CustomCompose != nil {
		inst.CustomCompose = *req.CustomCompose
	}

	// Docker resource limit
	if req.DockerMemory != nil {
		inst.DockerMemory = *req.DockerMemory
	}
	if req.DockerCPUs != nil {
		inst.DockerCPUs = *req.DockerCPUs
	}

	if err := s.db.UpdateInstance(inst); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("update failed: %v", err),
		})
	}

	s.log.Info("instance settings update complete", "instance_id", id)

	// audit log: change field each record
	s.auditFieldChange(c, "instance", id, inst.Name, "memory_min", oldInst.MemoryMin, inst.MemoryMin)
	s.auditFieldChange(c, "instance", id, inst.Name, "memory_max", oldInst.MemoryMax, inst.MemoryMax)
	s.auditFieldChange(c, "instance", id, inst.Name, "auto_start", fmt.Sprintf("%v", oldInst.AutoStart), fmt.Sprintf("%v", inst.AutoStart))
	s.auditFieldChange(c, "instance", id, inst.Name, "auto_restart", fmt.Sprintf("%v", oldInst.AutoRestart), fmt.Sprintf("%v", inst.AutoRestart))
	s.auditFieldChange(c, "instance", id, inst.Name, "stop_command", oldInst.StopCommand, inst.StopCommand)
	s.auditFieldChange(c, "instance", id, inst.Name, "jvm_args", oldInst.JVMArgs, inst.JVMArgs)
	s.auditFieldChange(c, "instance", id, inst.Name, "service_version", oldInst.ServiceVersion, inst.ServiceVersion)
	s.auditFieldChange(c, "instance", id, inst.Name, "java_version", oldInst.JavaVersion, inst.JavaVersion)
	s.auditFieldChange(c, "instance", id, inst.Name, "custom_dockerfile", oldInst.CustomDockerfile, inst.CustomDockerfile)
	s.auditFieldChange(c, "instance", id, inst.Name, "custom_compose", oldInst.CustomCompose, inst.CustomCompose)
	// typeper field
	s.auditFieldChange(c, "instance", id, inst.Name, "mysql_root_password", oldInst.MySQLRootPassword, inst.MySQLRootPassword)
	s.auditFieldChange(c, "instance", id, inst.Name, "mysql_extra_args", oldInst.MySQLExtraArgs, inst.MySQLExtraArgs)
	s.auditFieldChange(c, "instance", id, inst.Name, "pg_password", oldInst.PGPassword, inst.PGPassword)
	s.auditFieldChange(c, "instance", id, inst.Name, "pg_extra_args", oldInst.PGExtraArgs, inst.PGExtraArgs)
	s.auditFieldChange(c, "instance", id, inst.Name, "mongo_admin_user", oldInst.MongoAdminUser, inst.MongoAdminUser)
	s.auditFieldChange(c, "instance", id, inst.Name, "mongo_admin_password", oldInst.MongoAdminPassword, inst.MongoAdminPassword)
	s.auditFieldChange(c, "instance", id, inst.Name, "mongo_extra_args", oldInst.MongoExtraArgs, inst.MongoExtraArgs)
	s.auditFieldChange(c, "instance", id, inst.Name, "redis_password", oldInst.RedisPassword, inst.RedisPassword)
	s.auditFieldChange(c, "instance", id, inst.Name, "redis_extra_args", oldInst.RedisExtraArgs, inst.RedisExtraArgs)
	s.auditFieldChange(c, "instance", id, inst.Name, "kafka_broker_id", fmt.Sprintf("%d", oldInst.KafkaBrokerID), fmt.Sprintf("%d", inst.KafkaBrokerID))
	s.auditFieldChange(c, "instance", id, inst.Name, "kafka_extra_args", oldInst.KafkaExtraArgs, inst.KafkaExtraArgs)
	// backup schedule
	s.auditFieldChange(c, "instance", id, inst.Name, "backup_enabled", fmt.Sprintf("%v", oldInst.BackupEnabled), fmt.Sprintf("%v", inst.BackupEnabled))
	s.auditFieldChange(c, "instance", id, inst.Name, "backup_cron", oldInst.BackupCron, inst.BackupCron)
	s.auditFieldChange(c, "instance", id, inst.Name, "backup_max_count", fmt.Sprintf("%d", oldInst.BackupMaxCount), fmt.Sprintf("%d", inst.BackupMaxCount))
	// Docker resource limit
	s.auditFieldChange(c, "instance", id, inst.Name, "docker_memory", oldInst.DockerMemory, inst.DockerMemory)
	s.auditFieldChange(c, "instance", id, inst.Name, "docker_cpus", oldInst.DockerCPUs, inst.DockerCPUs)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":  "success",
		"message": "instance settings updated",
	})
}

// apiCreateInstance handles instance creation.
// 1. DB instance settings save (source of truth)
// 2. Minecraft type: agent JAR file transfer + inmemory register request
// 3. other type: DB saveonly (process manage Phase 2 from implementation)
func (s *Server) apiCreateInstance(c echo.Context) error {
	// multipart form parse
	agentID := c.FormValue("agent_id")
	name := c.FormValue("name")
	portStr := c.FormValue("port")
	instanceType := c.FormValue("instance_type")
	serviceVersion := c.FormValue("service_version")
	memMin := c.FormValue("memory_min")
	memMax := c.FormValue("memory_max")
	jvmArgsRaw := c.FormValue("jvm_args")
	autoStartStr := c.FormValue("auto_start")
	autoRestartStr := c.FormValue("auto_restart")
	startAfterStr := c.FormValue("start_after_create")
	javaPath := c.FormValue("java_path")

	// Java version + Docker customization
	javaVersion := c.FormValue("java_version")
	customDockerfile := c.FormValue("custom_dockerfile")
	customCompose := c.FormValue("custom_compose")

	// Docker resource limit
	dockerMemory := c.FormValue("docker_memory")
	dockerCPUs := c.FormValue("docker_cpus")

	// typeper field
	mysqlRootPassword := c.FormValue("mysql_root_password")
	mysqlDataDir := c.FormValue("mysql_data_dir")
	mysqlExtraArgs := c.FormValue("mysql_extra_args")
	pgPassword := c.FormValue("pg_password")
	pgDataDir := c.FormValue("pg_data_dir")
	pgExtraArgs := c.FormValue("pg_extra_args")
	mongoAdminUser := c.FormValue("mongo_admin_user")
	mongoAdminPassword := c.FormValue("mongo_admin_password")
	mongoDataDir := c.FormValue("mongo_data_dir")
	mongoExtraArgs := c.FormValue("mongo_extra_args")
	redisPassword := c.FormValue("redis_password")
	redisDataDir := c.FormValue("redis_data_dir")
	redisExtraArgs := c.FormValue("redis_extra_args")
	kafkaBrokerIDStr := c.FormValue("kafka_broker_id")
	kafkaDataDir := c.FormValue("kafka_data_dir")
	kafkaExtraArgs := c.FormValue("kafka_extra_args")

	// type default
	if instanceType == "" {
		instanceType = store.InstanceTypeMinecraft
	}
	if !store.ValidInstanceTypes[instanceType] {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": fmt.Sprintf("support not instance type: %s", instanceType),
		})
	}

	if agentID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "agent please select",
		})
	}
	if name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "instance name please enter",
		})
	}
	if !instanceNameRe.MatchString(name) {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "instance name English, number, hyphen(-), underscore(_), dot(.)only usable and English or number as must start ",
		})
	}

	// JAR or ZIP file read (Minecraft type fromonly required)
	var jarData []byte
	var jarFilename string
	var zipData []byte
	var zipFilename string

	// 1. JAR file check
	file, err := c.FormFile("server_jar")
	if err == nil {
		src, err := file.Open()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"status": "error", "message": "file open failed",
			})
		}
		defer src.Close()

		jarData, err = io.ReadAll(src)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"status": "error", "message": "file read failed",
			})
		}
		jarFilename = file.Filename
	}

	// 2. ZIP file check (existing server folder all upload)
	zipFile, err := c.FormFile("server_zip")
	if err == nil {
		src, err := zipFile.Open()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"status": "error", "message": "ZIP file open failed",
			})
		}
		defer src.Close()

		zipData, err = io.ReadAll(src)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"status": "error", "message": "ZIP file read failed",
			})
		}
		zipFilename = zipFile.Filename
	}

	// JAR field as uploadedonly extension .zipin case ZIP as remaining
	if len(jarData) > 0 && len(zipData) == 0 && strings.HasSuffix(strings.ToLower(jarFilename), ".zip") {
		zipData = jarData
		zipFilename = jarFilename
		jarData = nil
		jarFilename = ""
	}

	// Minecraft type: JAR or ZIP at least one is required
	if instanceType == store.InstanceTypeMinecraft && len(jarData) == 0 && len(zipData) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "server JAR file or server ZIP file uploadplease",
		})
	}

	// port parse (typeper default)
	port := 0
	if portStr != "" {
		if p, err := parseIntSafe(portStr); err == nil && p > 0 {
			port = p
		}
	}
	if port == 0 {
		switch instanceType {
		case store.InstanceTypeMinecraft:
			port = 25565
		case store.InstanceTypeMySQL:
			port = 3306
		case store.InstanceTypePostgreSQL:
			port = 5432
		case store.InstanceTypeMongoDB:
			port = 27017
		case store.InstanceTypeRedis:
			port = 6379
		case store.InstanceTypeKafka:
			port = 9092
		default:
			port = 25565
		}
	}

	// default apply
	if memMin == "" {
		memMin = "512M"
	}
	if memMax == "" {
		memMax = "1024M"
	}
	if javaPath == "" {
		javaPath = "java"
	}

	autoStart := autoStartStr == "true" || autoStartStr == "on"
	autoRestart := autoRestartStr == "true" || autoRestartStr == "on"
	startAfter := startAfterStr == "true" || startAfterStr == "on"

	// JVM flag cleanup
	jvmArgsStr := ""
	if jvmArgsRaw != "" {
		var cleaned []string
		for _, line := range strings.Split(jvmArgsRaw, "\n") {
			arg := strings.TrimSpace(line)
			if arg != "" {
				cleaned = append(cleaned, arg)
			}
		}
		jvmArgsStr = strings.Join(cleaned, "\n")
	}

	// Kafka broker ID parse
	kafkaBrokerID := 0
	if kafkaBrokerIDStr != "" {
		if bid, err := parseIntSafe(kafkaBrokerIDStr); err == nil {
			kafkaBrokerID = bid
		}
	}

	// instance ID create
	instID := fmt.Sprintf("%s-%s", agentID, name)
	workDir := fmt.Sprintf("./servers/%s", name)

	// 1. DB instance save (source of truth)
	serverJar := jarFilename
	if serverJar == "" {
		serverJar = "server.jar" // default (non-Minecraft type from notuse)
	}

	inst := &store.Instance{
		ID:              instID,
		NodeID:          agentID,
		Name:            name,
		Port:            port,
		MemoryMin:       memMin,
		MemoryMax:       memMax,
		JavaPath:        javaPath,
		ServerJar:       serverJar,
		WorkDir:         workDir,
		Status:          "stopped",
		AutoStart:       autoStart,
		AutoRestart:     autoRestart,
		RestartDelaySec: 10,
		StopCommand:     "stop",
		JVMArgs:         jvmArgsStr,
		AcceptEULA:      true,
		InstanceType:    instanceType,
		ServiceVersion:  serviceVersion,

		// typeper field
		MySQLRootPassword:  mysqlRootPassword,
		MySQLDataDir:       mysqlDataDir,
		MySQLExtraArgs:     mysqlExtraArgs,
		PGPassword:         pgPassword,
		PGDataDir:          pgDataDir,
		PGExtraArgs:        pgExtraArgs,
		MongoAdminUser:     mongoAdminUser,
		MongoAdminPassword: mongoAdminPassword,
		MongoDataDir:       mongoDataDir,
		MongoExtraArgs:     mongoExtraArgs,
		RedisPassword:      redisPassword,
		RedisDataDir:       redisDataDir,
		RedisExtraArgs:     redisExtraArgs,
		KafkaBrokerID:      kafkaBrokerID,
		KafkaDataDir:       kafkaDataDir,
		KafkaExtraArgs:     kafkaExtraArgs,

		JavaVersion:      javaVersion,
		CustomDockerfile: customDockerfile,
		CustomCompose:    customCompose,

		DockerMemory: dockerMemory,
		DockerCPUs:   dockerCPUs,
	}

	if err := s.db.CreateInstance(inst); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status": "error", "message": fmt.Sprintf("DB save failed: %v", err),
		})
	}

	s.log.Info("instance DB save complete",
		"instance_id", instID,
		"agent_id", agentID,
		"name", name,
		"type", instanceType,
	)

	// audit log: create instance
	s.audit(c, "create", "instance", instID, name, "", "", "",
		fmt.Sprintf("create instance: %s (type: %s, node: %s, port: %d)", name, instanceType, agentID, port))

	// 2. default network ensure (DB + agent)
	networkName := "craftstack-default"
	s.ensureDefaultNetwork(agentID, networkName)

	// 3. agent send (the agent onlineday only when)
	agentAddr, ok := s.connector.GetAgentAddress(agentID)
	if ok {
		conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			defer conn.Close()

			// 3a. CreateInstance RPC — metadataonly send (binary data none)
			// file streaming neededif done start delay (upload complete after start)
			needsUpload := len(jarData) > 0 || len(zipData) > 0
			deferStart := startAfter && needsUpload

			ctx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
			defer cancel()

			agentClient := pb.NewAgentServiceClient(conn)
			resp, err := agentClient.CreateInstance(ctx, &pb.CreateInstanceRequest{
				Name:             name,
				Port:             int32(port),
				MemoryMin:        memMin,
				MemoryMax:        memMax,
				ServerJarName:    serverJar,
				ServerJarData:    nil, // streaming as send
				AutoStart:        autoStart,
				AutoRestart:      autoRestart,
				AcceptEula:       true,
				StartAfterCreate: startAfter && !needsUpload, // file upload neededwhen start delay
				JavaPath:         javaPath,
				JvmArgs:          inst.JVMArgsList(),
				InstanceType:     instanceType,
				ServiceVersion:   serviceVersion,
				JavaVersion:      javaVersion,
				CustomDockerfile: customDockerfile,
				CustomCompose:    customCompose,
				NetworkName:      networkName,
				ServerZipName:    zipFilename,
				ServerZipData:    nil, // streaming as send
			})
			if err != nil {
				s.log.Warn("agent create instance send failed (next heartbeat from retry)", "error", err)
			} else if !resp.Success {
				s.log.Warn("agent create instance failed", "message", resp.Message)
			}

			// 3b. UploadServerData streaming — file data 4MB chunk as send
			if err == nil && needsUpload {
				uploadErr := s.streamUploadServerData(c.Request().Context(), agentClient, name, jarFilename, jarData, zipFilename, zipData)
				if uploadErr != nil {
					s.log.Warn("file streaming upload failed", "error", uploadErr)
				} else if deferStart {
					// upload complete after start instance
					startCtx, startCancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
					defer startCancel()
					_, startErr := agentClient.ControlInstance(startCtx, &pb.ControlInstanceRequest{
						AgentId:    agentID,
						InstanceId: instID,
						Action:     pb.InstanceAction_INSTANCE_ACTION_START,
					})
					if startErr != nil {
						s.log.Warn("upload after start instance failed", "error", startErr)
					}
				}
			}
		}
	} else {
		s.log.Info("agent offline, next connect when instance sync e.g.", "agent_id", agentID)
	}

	// 4. DB instance-network mapping save
	networkID := fmt.Sprintf("%s-%s", agentID, networkName)
	containerName := fmt.Sprintf("craftstack-%s", name)
	if err := s.db.AddInstanceToNetwork(instID, networkID, containerName, ""); err != nil {
		s.log.Warn("instance-network mapping save failed", "error", err)
	}

	// 5. mesh DNS record auto register
	if s.mesh != nil {
		go s.mesh.RegisterInstanceDNS(inst)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":      "success",
		"message":     "the instance created",
		"instance_id": instID,
	})
}

// --- Instance Metrics API ---

// apiInstanceMetrics returns current + historical metrics for an instance.
func (s *Server) apiInstanceMetrics(c echo.Context) error {
	id := c.Param("id")

	// Current metrics from cache
	var current *InstanceMetrics
	if mc, ok := s.connector.(InstanceMetricsProvider); ok {
		current = mc.GetInstanceMetrics(id)
	}

	// Historical metrics from DB (last 60 records = ~5 min at 5s intervals)
	history, _ := s.db.ListInstanceMetrics(id, 60)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"current": current,
		"history": history,
	})
}

// enrichInstanceNetworkData adds per-instance network info (WG IP, mesh DNS, networks) to the data map.
// Used by handleInstances and htmxInstancesTable to display network columns.
func (s *Server) enrichInstanceNetworkData(data map[string]interface{}, instances []*store.Instance) {
	if len(instances) == 0 {
		return
	}

	// 1. unique NodeID collect → Node query WG Address map configuration
	nodeWGMap := make(map[string]string) // nodeID → WG IP (without CIDR)
	seen := make(map[string]bool)
	for _, inst := range instances {
		if !seen[inst.NodeID] {
			seen[inst.NodeID] = true
			if node, err := s.db.GetNode(inst.NodeID); err == nil && node.WGAddress != "" {
				wgIP := node.WGAddress
				if idx := strings.Index(wgIP, "/"); idx > 0 {
					wgIP = wgIP[:idx]
				}
				nodeWGMap[inst.NodeID] = wgIP
			}
		}
	}

	// 2. each instance connect network count query
	instNetCountMap := make(map[string]int) // instanceID → network count
	for _, inst := range instances {
		nets, err := s.db.ListInstanceNetworks(inst.ID)
		if err == nil {
			instNetCountMap[inst.ID] = len(nets)
		}
	}

	data["NodeWGMap"] = nodeWGMap
	data["InstNetCountMap"] = instNetCountMap
}

// ensureDefaultNetwork ensures the default network exists in DB and on the agent.
// Called BEFORE container creation so that docker create --network works.
func (s *Server) ensureDefaultNetwork(agentID, networkName string) {
	networkID := fmt.Sprintf("%s-%s", agentID, networkName)

	// DB from network query, if absent create
	_, err := s.db.GetNetwork(networkID)
	if err != nil {
		if err := s.db.CreateNetwork(&store.Network{
			ID:     networkID,
			Name:   networkName,
			Driver: "bridge",
			NodeID: agentID,
		}); err != nil {
			s.log.Warn("default network DB create failed", "error", err)
			return
		}
		s.log.Info("default network DB create complete", "network_id", networkID)
	}

	// agent create network request (already existing ignore)
	if agentAddr, ok := s.connector.GetAgentAddress(agentID); ok {
		conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			agentClient := pb.NewAgentServiceClient(conn)
			agentClient.CreateNetwork(ctx, &pb.CreateNetworkRequest{
				Name:   networkName,
				Driver: "bridge",
			})
		}
	}
}

// streamUploadServerData sends a JAR or ZIP file to the agent via gRPC client streaming.
// Files are sent in 4MB chunks to avoid the default 64MB gRPC message size limit.
func (s *Server) streamUploadServerData(ctx context.Context, agentClient pb.AgentServiceClient, instanceName string, jarName string, jarData []byte, zipName string, zipData []byte) error {
	const chunkSize = 4 * 1024 * 1024 // 4MB

	// some file to send decide
	var fileType, fileName string
	var data []byte

	if len(zipData) > 0 {
		fileType = "zip"
		fileName = zipName
		data = zipData
	} else if len(jarData) > 0 {
		fileType = "jar"
		fileName = jarName
		data = jarData
	} else {
		return nil // to send file none
	}

	s.log.Info("file streaming upload start",
		"instance", instanceName,
		"type", fileType,
		"file", fileName,
		"size", len(data),
	)

	// streaming , so timeout generously (10min)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	stream, err := agentClient.UploadServerData(streamCtx)
	if err != nil {
		return fmt.Errorf("streaming start failed: %w", err)
	}

	totalSize := int64(len(data))
	sent := 0

	for sent < len(data) {
		end := sent + chunkSize
		if end > len(data) {
			end = len(data)
		}

		req := &pb.UploadServerDataRequest{
			ChunkData: data[sent:end],
		}

		// first th chunk metadata include
		if sent == 0 {
			req.InstanceName = instanceName
			req.FileType = fileType
			req.FileName = fileName
			req.TotalSize = totalSize
		}

		if err := stream.Send(req); err != nil {
			return fmt.Errorf("chunk send failed (offset=%d): %w", sent, err)
		}

		sent = end
	}

	// streaming shutdown and response receive
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("streaming shutdown failed: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("agent file handle failed: %s", resp.Message)
	}

	s.log.Info("file streaming upload complete",
		"instance", instanceName,
		"type", fileType,
		"detected_jar", resp.DetectedJar,
	)

	return nil
}

// htmxInstanceMetrics returns instance metrics partial for HTMX polling.
func (s *Server) htmxInstanceMetrics(c echo.Context) error {
	id := c.Param("id")
	var current *InstanceMetrics
	if mc, ok := s.connector.(InstanceMetricsProvider); ok {
		current = mc.GetInstanceMetrics(id)
	}
	data := map[string]interface{}{
		"InstanceID":      id,
		"InstanceMetrics": current,
	}
	return renderPartial(c, "instance_metrics", data)
}

func (s *Server) htmxInstancesTable(c echo.Context) error {
	instances, err := s.db.ListInstances("")
	if err != nil {
		return c.HTML(http.StatusInternalServerError, `<div class="alert alert-error">instance list load cannot</div>`)
	}
	instances = s.visibleInstances(c, instances)
	s.overlayInstanceStatus(instances)
	data := map[string]interface{}{"Instances": instances}
	s.enrichInstanceNetworkData(data, instances)
	return renderPartial(c, "instances_table", data)
}

// htmxInstanceStatus returns the current instance status with control buttons.
func (s *Server) htmxInstanceStatus(c echo.Context) error {
	id := c.Param("id")
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return c.HTML(http.StatusNotFound, "")
	}
	if !s.connector.IsAgentOnline(inst.NodeID) {
		inst.Status = "offline"
	}
	return renderPartial(c, "instance_status", map[string]interface{}{"Instance": inst})
}

func (s *Server) htmxDashboardStats(c echo.Context) error {
	nodes, _ := s.db.ListNodes()
	s.overlayNodeStatus(nodes)
	instances, _ := s.db.ListInstances("")
	instances = s.visibleInstances(c, instances)
	s.overlayInstanceStatus(instances)

	onlineNodes := 0
	for _, n := range nodes {
		if n.Status == "online" {
			onlineNodes++
		}
	}
	runningInstances := 0
	for _, i := range instances {
		if i.Status == "running" {
			runningInstances++
		}
	}

	allSync, _ := s.db.ListSyncHistory(0)
	totalBackups := 0
	for _, inst := range instances {
		count, _ := s.db.CountBackups(inst.ID)
		totalBackups += count
	}

	return renderPartial(c, "dashboard_stats", map[string]interface{}{
		"TotalNodes":       len(nodes),
		"OnlineNodes":      onlineNodes,
		"TotalInstances":   len(instances),
		"RunningInstances": runningInstances,
		"TotalSyncs":       len(allSync),
		"TotalBackups":     totalBackups,
	})
}
