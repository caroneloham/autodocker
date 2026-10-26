package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var (
	DockerClient *client.Client
	dockerMu     sync.Mutex
)

// InitDocker creates the Docker client singleton — retries if previously failed
func InitDocker() *client.Client {
	dockerMu.Lock()
	defer dockerMu.Unlock()

	// Already connected and working
	if DockerClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := DockerClient.Ping(ctx); err == nil {
			return DockerClient
		}
		// Connection lost, try to reconnect
		DockerClient = nil
	}

	// Local installations use only a local socket or named pipe. Never probe
	// unauthenticated Docker TCP endpoints during automatic discovery.
	setup, setupErr := ReadLocalSetup()
	endpoints := []string{}
	if setupErr == nil && setup.DockerHost != "" && setup.DockerHost != "auto" {
		endpoints = append(endpoints, setup.DockerHost)
	} else if runtime.GOOS == "windows" {
		endpoints = append(endpoints,
			"npipe:////./pipe/dockerDesktopLinuxEngine",
			"npipe:////./pipe/docker_engine",
		)
	} else {
		endpoints = append(endpoints,
			"unix:///var/run/docker.sock",
		)
	}

	for _, ep := range endpoints {
		opts := []client.Opt{client.WithHost(ep), client.WithAPIVersionNegotiation()}
		var err error
		DockerClient, err = client.NewClientWithOpts(opts...)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err = DockerClient.Ping(ctx)
		cancel()
		if err == nil {
			fmt.Printf("[OK] Docker connecte avec succes (via %s)\n", ep)
			return DockerClient
		}
	}

	fmt.Println("[WARN] Docker non connecte: aucun endpoint accessible")
	DockerClient = nil
	return nil
}

// ReconnectDocker applies an owner's newly saved local connection choice.
func ReconnectDocker() *client.Client {
	dockerMu.Lock()
	old := DockerClient
	DockerClient = nil
	dockerMu.Unlock()
	if old != nil {
		old.Close()
	}
	return InitDocker()
}

// EnsureDocker retries Docker connection if not connected
func EnsureDocker() *client.Client {
	dockerMu.Lock()
	current := DockerClient
	dockerMu.Unlock()

	if current != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := current.Ping(ctx)
		cancel()
		if err == nil {
			return current
		}
		dockerMu.Lock()
		if DockerClient == current {
			DockerClient = nil
		}
		dockerMu.Unlock()
	}
	return InitDocker()
}

// StatsResult holds container resource usage
type StatsResult struct {
	CPU      float64
	MemMB    float64
	MemLimMB float64
	NetRx    int64
	NetTx    int64
}

// CollectContainerStats gets resource usage for a single container
func CollectContainerStats(ctx context.Context, cli *client.Client, containerID string) (*StatsResult, error) {
	resp, err := cli.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var stats container.StatsResponse
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, err
	}

	result := &StatsResult{}

	// CPU percent
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
	numCPUs := float64(stats.CPUStats.OnlineCPUs)
	if numCPUs == 0 {
		numCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if numCPUs == 0 {
		numCPUs = 1
	}
	if sysDelta > 0 {
		result.CPU = (cpuDelta / sysDelta) * numCPUs * 100.0
	}

	// Memory
	result.MemMB = float64(stats.MemoryStats.Usage) / 1048576.0
	result.MemLimMB = float64(stats.MemoryStats.Limit) / 1048576.0

	// Network
	for _, v := range stats.Networks {
		result.NetRx += int64(v.RxBytes)
		result.NetTx += int64(v.TxBytes)
	}

	return result, nil
}
