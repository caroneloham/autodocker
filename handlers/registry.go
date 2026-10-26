package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// RegisterRegistryRoutes adds image update check routes to the tools group.
func RegisterRegistryRoutes(api *gin.RouterGroup) {
	api.GET("/images/check-updates", handleCheckImageUpdates)
}

type dockerHubTagResult struct {
	Name        string `json:"name"`
	LastUpdated string `json:"last_updated"`
	Digest      string `json:"digest"`
}

type dockerHubResponse struct {
	Results []dockerHubTagResult `json:"results"`
}

// GET /api/images/check-updates
func handleCheckImageUpdates(c *gin.Context) {
	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(503, gin.H{"error": "Docker non connecte"})
		return
	}

	// Get all running images
	containers, err := cli.ContainerList(c.Request.Context(), container.ListOptions{All: false})
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	seen := make(map[string]bool)
	results := make([]gin.H, 0)

	for _, ctr := range containers {
		image := ctr.Image
		if seen[image] { continue }
		seen[image] = true

		// Parse image into namespace/repo:tag
		ns, repo, tag := parseImageRef(image)
		if repo == "" { continue }

		// Query Docker Hub
		hubTag, hubDate, err := queryDockerHub(ns, repo, tag)
		if err != nil {
			results = append(results, gin.H{
				"image":            image,
				"current_tag":      tag,
				"update_available": false,
				"error":            err.Error(),
			})
			continue
		}

		// Check if local image is older
		localCreated := ""
		inspect, _, ierr := cli.ImageInspectWithRaw(c.Request.Context(), image)
		if ierr == nil {
			localCreated = inspect.Created
		}

		updateAvailable := false
		if hubDate != "" && localCreated != "" {
			hubTime, _ := time.Parse(time.RFC3339Nano, hubDate)
			localTime, _ := time.Parse(time.RFC3339Nano, localCreated)
			if !hubTime.IsZero() && !localTime.IsZero() {
				updateAvailable = hubTime.After(localTime)
			}
		}

		results = append(results, gin.H{
			"image":            image,
			"current_tag":      tag,
			"hub_tag":          hubTag,
			"hub_updated":      hubDate,
			"local_created":    localCreated,
			"update_available": updateAvailable,
		})
	}

	c.JSON(200, gin.H{"status": "ok", "images": results})
}

func parseImageRef(image string) (namespace, repo, tag string) {
	// Remove registry prefix if present
	if strings.Contains(image, "/") && strings.Count(image, "/") >= 2 {
		// e.g., ghcr.io/owner/repo:tag — skip non-Docker Hub
		parts := strings.SplitN(image, "/", 2)
		if strings.Contains(parts[0], ".") {
			return "", "", "" // not Docker Hub
		}
	}

	tag = "latest"
	ref := image
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		tag = ref[idx+1:]
		ref = ref[:idx]
	}

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		// Official image: nginx, mariadb, etc.
		namespace = "library"
		repo = parts[0]
	} else {
		namespace = parts[0]
		repo = parts[1]
	}
	return
}

func queryDockerHub(namespace, repo, tag string) (latestTag, lastUpdated string, err error) {
	url := fmt.Sprintf("https://registry.hub.docker.com/v2/repositories/%s/%s/tags?page_size=5&ordering=last_updated&name=%s",
		namespace, repo, tag)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", "", fmt.Errorf("Hub inaccessible")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("Hub HTTP %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var hubResp dockerHubResponse
	if err := json.Unmarshal(body, &hubResp); err != nil {
		return "", "", fmt.Errorf("JSON parse error")
	}

	if len(hubResp.Results) == 0 {
		return "", "", fmt.Errorf("Tag '%s' non trouve", tag)
	}

	// Find matching tag
	for _, r := range hubResp.Results {
		if r.Name == tag {
			return r.Name, r.LastUpdated, nil
		}
	}

	// Return first result
	return hubResp.Results[0].Name, hubResp.Results[0].LastUpdated, nil
}
