package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/gin-gonic/gin"
)

// RegisterFlowMapRoutes registers the network flow map endpoint.
func RegisterFlowMapRoutes(api *gin.RouterGroup) {
	api.GET("/network/flow", handleNetworkFlow)
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type flowNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image,omitempty"`
	Project string `json:"project,omitempty"`
	Service string `json:"service,omitempty"`
	State   string `json:"state,omitempty"`
	Subnet  string `json:"subnet,omitempty"`
	Type    string `json:"type"` // "container" or "network"
}

type flowEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	IP     string `json:"ip"`
	Type   string `json:"type"` // "connection"
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func handleNetworkFlow(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "Client Docker non disponible"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	// List running containers
	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	var nodes []flowNode
	for _, cont := range containers {
		name := ""
		if len(cont.Names) > 0 {
			name = strings.TrimPrefix(cont.Names[0], "/")
		}
		labels := cont.Labels
		project := ""
		service := ""
		if labels != nil {
			project = labels["com.docker.compose.project"]
			service = labels["com.docker.compose.service"]
		}
		imageName := cont.Image
		shortID := cont.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		nodes = append(nodes, flowNode{
			ID:      shortID,
			Name:    name,
			Image:   imageName,
			Project: project,
			Service: service,
			State:   cont.State,
			Type:    "container",
		})
	}

	// List networks
	networks, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		networks = nil
	}

	var networkNodes []flowNode
	var edges []flowEdge

	for _, net := range networks {
		if net.Name == "bridge" || net.Name == "host" || net.Name == "none" {
			continue
		}

		containersInNet := net.Containers
		if len(containersInNet) == 0 {
			continue
		}

		// Subnet
		subnet := ""
		if len(net.IPAM.Config) > 0 {
			subnet = net.IPAM.Config[0].Subnet
		}

		shortNetID := net.ID
		if len(shortNetID) > 12 {
			shortNetID = shortNetID[:12]
		}
		netNodeID := "net_" + shortNetID

		networkNodes = append(networkNodes, flowNode{
			ID:     netNodeID,
			Name:   net.Name,
			Subnet: subnet,
			Type:   "network",
		})

		for cid, cinfo := range containersInNet {
			shortCID := cid
			if len(shortCID) > 12 {
				shortCID = shortCID[:12]
			}
			ip := ""
			if cinfo.IPv4Address != "" {
				ip = strings.SplitN(cinfo.IPv4Address, "/", 2)[0]
			}
			edges = append(edges, flowEdge{
				Source: shortCID,
				Target: netNodeID,
				IP:     ip,
				Type:   "connection",
			})
		}
	}

	allNodes := append(nodes, networkNodes...)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"nodes":  allNodes,
		"edges":  edges,
	})
}
