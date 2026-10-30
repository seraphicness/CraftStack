// Package observability exposes CraftStack metrics to Prometheus and InfluxDB.
//
// The Prometheus exporter emits the text exposition format directly — no
// client_golang dependency — by reading snapshots from a MetricsSource.
// The infrastructure package provisions the optional managed monitoring stack.
package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// NodeSnapshot is a single node's cached metrics.
type NodeSnapshot struct {
	NodeID      string
	CPUPercent  float64
	MemPercent  float64
	MemUsedMB   int64
	MemTotalMB  int64
	DiskPercent float64
	DiskUsedMB  int64
	DiskTotalMB int64
	UpdatedAt   time.Time
}

// InstanceSnapshot is a single instance's cached metrics.
type InstanceSnapshot struct {
	InstanceID      string
	NodeID          string
	Type            string
	CPUPercent      float64
	MemoryUsedMB    int64
	MemoryLimitMB   int64
	NetRxBytes      int64
	NetTxBytes      int64
	BlockReadBytes  int64
	BlockWriteBytes int64
	UpdatedAt       time.Time
}

// MetricsSource provides snapshots to the exporter and InfluxDB pusher.
type MetricsSource interface {
	NodeSnapshots() []NodeSnapshot
	InstanceSnapshots() []InstanceSnapshot
}

// PrometheusHandler returns an http.Handler emitting the Prometheus text format.
func PrometheusHandler(src MetricsSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeProm(w, src)
	})
}

func writeProm(w io.Writer, src MetricsSource) {
	nodes := src.NodeSnapshots()
	insts := src.InstanceSnapshots()

	// Stable output order for diffability.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	sort.Slice(insts, func(i, j int) bool { return insts[i].InstanceID < insts[j].InstanceID })

	// Nodes
	writeHelp(w, "craftstack_node_cpu_percent", "gauge", "Node CPU utilization percent.")
	for _, n := range nodes {
		fmt.Fprintf(w, `craftstack_node_cpu_percent{node=%q} %g`+"\n", n.NodeID, n.CPUPercent)
	}
	writeHelp(w, "craftstack_node_memory_percent", "gauge", "Node memory utilization percent.")
	for _, n := range nodes {
		fmt.Fprintf(w, `craftstack_node_memory_percent{node=%q} %g`+"\n", n.NodeID, n.MemPercent)
	}
	writeHelp(w, "craftstack_node_memory_used_mb", "gauge", "Node memory used, MB.")
	for _, n := range nodes {
		fmt.Fprintf(w, `craftstack_node_memory_used_mb{node=%q} %d`+"\n", n.NodeID, n.MemUsedMB)
	}
	writeHelp(w, "craftstack_node_memory_total_mb", "gauge", "Node memory total, MB.")
	for _, n := range nodes {
		fmt.Fprintf(w, `craftstack_node_memory_total_mb{node=%q} %d`+"\n", n.NodeID, n.MemTotalMB)
	}
	writeHelp(w, "craftstack_node_disk_percent", "gauge", "Node disk utilization percent.")
	for _, n := range nodes {
		fmt.Fprintf(w, `craftstack_node_disk_percent{node=%q} %g`+"\n", n.NodeID, n.DiskPercent)
	}

	// Instances
	writeHelp(w, "craftstack_instance_cpu_percent", "gauge", "Instance CPU percent.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_cpu_percent{instance=%q,node=%q,type=%q} %g`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.CPUPercent)
	}
	writeHelp(w, "craftstack_instance_memory_used_mb", "gauge", "Instance memory used, MB.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_memory_used_mb{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.MemoryUsedMB)
	}
	writeHelp(w, "craftstack_instance_memory_limit_mb", "gauge", "Instance memory limit, MB.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_memory_limit_mb{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.MemoryLimitMB)
	}
	writeHelp(w, "craftstack_instance_net_rx_bytes", "counter", "Instance network rx bytes.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_net_rx_bytes{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.NetRxBytes)
	}
	writeHelp(w, "craftstack_instance_net_tx_bytes", "counter", "Instance network tx bytes.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_net_tx_bytes{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.NetTxBytes)
	}
	writeHelp(w, "craftstack_instance_block_read_bytes", "counter", "Instance block device read bytes.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_block_read_bytes{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.BlockReadBytes)
	}
	writeHelp(w, "craftstack_instance_block_write_bytes", "counter", "Instance block device write bytes.")
	for _, in := range insts {
		fmt.Fprintf(w, `craftstack_instance_block_write_bytes{instance=%q,node=%q,type=%q} %d`+"\n",
			in.InstanceID, in.NodeID, in.Type, in.BlockWriteBytes)
	}

	fmt.Fprintf(w, "# EOF\n")
	_ = strings.Builder{} // silence unused import on older toolchains
}

func writeHelp(w io.Writer, name, kind, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}
