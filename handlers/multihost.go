package handlers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"autodocker-go/db"
	"autodocker-go/middleware"
	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// clientCache stores Docker clients for remote hosts.
var (
	clientCache   = make(map[string]*client.Client)
	clientCacheMu sync.RWMutex
)

// RegisterMultihostRoutes registers all multi-host Docker management routes.
func RegisterMultihostRoutes(api *gin.RouterGroup) {
	g := api.Group("/hosts")
	{
		g.GET("", handleListHosts)
		g.POST("", middleware.AdminRequired(), handleAddHost)
		g.DELETE("/:id", middleware.AdminRequired(), handleDeleteHost)
		g.POST("/:id/test", handleTestHost)
		g.GET("/:id/containers", handleHostContainers)
		g.GET("/:id/info", handleHostInfo)
	}
}

// ---------------------------------------------------------------------------
// Helper: get Docker client for a given host
// ---------------------------------------------------------------------------

func getDockerClientForHost(hostID string) (*client.Client, error) {
	if hostID == "" || hostID == "local" {
		return services.EnsureDocker(), nil
	}

	clientCacheMu.RLock()
	if cached, ok := clientCache[hostID]; ok {
		clientCacheMu.RUnlock()
		return cached, nil
	}
	clientCacheMu.RUnlock()

	var url, authType, tlsCa, tlsCert, tlsKey string
	err := db.DB.QueryRow(
		"SELECT url, auth_type, tls_ca, tls_cert, tls_key FROM docker_hosts WHERE host_id=?", hostID,
	).Scan(&url, &authType, &tlsCa, &tlsCert, &tlsKey)
	if err != nil {
		return nil, fmt.Errorf("hote non trouve")
	}

	opts := []client.Opt{
		client.WithHost(url),
		client.WithAPIVersionNegotiation(),
	}

	if authType == "tls" && tlsCert != "" && tlsKey != "" {
		cert, err := tls.X509KeyPair([]byte(tlsCert), []byte(tlsKey))
		if err != nil {
			return nil, fmt.Errorf("erreur chargement certificat TLS: %w", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		if tlsCa != "" {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM([]byte(tlsCa))
			tlsConfig.RootCAs = pool
		}
		httpClient := &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		}
		opts = append(opts, client.WithHTTPClient(httpClient))
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("erreur creation client Docker: %w", err)
	}

	clientCacheMu.Lock()
	clientCache[hostID] = cli
	clientCacheMu.Unlock()

	return cli, nil
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func handleListHosts(c *gin.Context) {
	var hosts []gin.H

	// Local host
	localEntry := gin.H{
		"host_id":          "local",
		"name":             "Local",
		"url":              "unix:///var/run/docker.sock",
		"auth_type":        "none",
		"status":           "unknown",
		"last_check":       nil,
		"containers_count": 0,
	}
	if services.EnsureDocker() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := services.EnsureDocker().Ping(ctx)
		cancel()
		if err == nil {
			localEntry["status"] = "online"
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			containers, err := services.EnsureDocker().ContainerList(ctx2, container.ListOptions{})
			cancel2()
			if err == nil {
				localEntry["containers_count"] = len(containers)
			}
		} else {
			localEntry["status"] = "offline"
		}
	}
	hosts = append(hosts, localEntry)

	// Remote hosts
	rows, err := db.DB.Query(
		"SELECT host_id, name, url, auth_type, status, last_check, added_by, added_at FROM docker_hosts ORDER BY name",
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var hostID, name, url, authType, status, lastCheck, addedBy, addedAt string
			rows.Scan(&hostID, &name, &url, &authType, &status, &lastCheck, &addedBy, &addedAt)
			entry := gin.H{
				"host_id":          hostID,
				"name":             name,
				"url":              url,
				"auth_type":        authType,
				"status":           status,
				"last_check":       lastCheck,
				"added_by":         addedBy,
				"added_at":         addedAt,
				"containers_count": 0,
			}
			if status == "online" {
				if cli, err := getDockerClientForHost(hostID); cli != nil && err == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					containers, err := cli.ContainerList(ctx, container.ListOptions{})
					cancel()
					if err == nil {
						entry["containers_count"] = len(containers)
					}
				}
			}
			hosts = append(hosts, entry)
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "hosts": hosts})
}

func handleAddHost(c *gin.Context) {
	var body struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		AuthType string `json:"auth_type"`
		TLSCa    string `json:"tls_ca"`
		TLSCert  string `json:"tls_cert"`
		TLSKey   string `json:"tls_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "JSON invalide"})
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.AuthType == "" {
		body.AuthType = "none"
	}

	if body.Name == "" || body.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Les champs 'name' et 'url' sont requis"})
		return
	}

	if !strings.HasPrefix(body.URL, "tcp://") && !strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "ssh://") {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "L'URL doit commencer par tcp://, https:// ou ssh://"})
		return
	}

	// Test connection
	opts := []client.Opt{
		client.WithHost(body.URL),
		client.WithAPIVersionNegotiation(),
	}
	if body.AuthType == "tls" && body.TLSCert != "" && body.TLSKey != "" {
		cert, err := tls.X509KeyPair([]byte(body.TLSCert), []byte(body.TLSKey))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Certificat TLS invalide: %v", err)})
			return
		}
		tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}
		if body.TLSCa != "" {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM([]byte(body.TLSCa))
			tlsConfig.RootCAs = pool
		}
		httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
		opts = append(opts, client.WithHTTPClient(httpClient))
	}

	testCli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Connexion echouee: %v", err)})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = testCli.Ping(ctx)
	cancel()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": fmt.Sprintf("Connexion echouee: %v", err)})
		return
	}

	hostID := fmt.Sprintf("%x", time.Now().UnixNano())
	nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")
	s := sessions.Default(c)
	username, _ := s.Get("user").(string)

	_, err = db.DB.Exec(
		"INSERT INTO docker_hosts (host_id, name, url, auth_type, tls_ca, tls_cert, tls_key, status, last_check, added_by, added_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		hostID, body.Name, body.URL, body.AuthType, body.TLSCa, body.TLSCert, body.TLSKey, "online", nowStr, username, nowStr,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	db.LogActivity(username, "add_host", body.Name, fmt.Sprintf("URL: %s", body.URL))
	c.JSON(http.StatusCreated, gin.H{"status": "ok", "host_id": hostID, "name": body.Name, "url": body.URL, "connection": "online"})
}

func handleDeleteHost(c *gin.Context) {
	hostID := c.Param("id")
	if hostID == "local" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Impossible de supprimer l'hote local"})
		return
	}

	// Remove from cache
	clientCacheMu.Lock()
	delete(clientCache, hostID)
	clientCacheMu.Unlock()

	result, err := db.DB.Exec("DELETE FROM docker_hosts WHERE host_id=?", hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Hote non trouve"})
		return
	}

	s := sessions.Default(c)
	username, _ := s.Get("user").(string)
	db.LogActivity(username, "delete_host", hostID, "")
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Hote supprime"})
}

func handleTestHost(c *gin.Context) {
	hostID := c.Param("id")

	if hostID == "local" {
		if services.EnsureDocker() == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "Client Docker local non disponible"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := services.EnsureDocker().Ping(ctx)
		cancel()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := services.EnsureDocker().Info(ctx2)
		cancel2()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":           "online",
			"version":          info.ServerVersion,
			"containers_count": info.Containers,
		})
		return
	}

	// Invalidate cache
	clientCacheMu.Lock()
	delete(clientCache, hostID)
	clientCacheMu.Unlock()

	cli, err := getDockerClientForHost(hostID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = cli.Ping(ctx)
	cancel()
	nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")

	if err != nil {
		db.DB.Exec("UPDATE docker_hosts SET status='error', last_check=? WHERE host_id=?", nowStr, hostID)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	info, _ := cli.Info(ctx2)
	cancel2()

	db.DB.Exec("UPDATE docker_hosts SET status='online', last_check=? WHERE host_id=?", nowStr, hostID)
	c.JSON(http.StatusOK, gin.H{
		"status":           "online",
		"version":          info.ServerVersion,
		"containers_count": info.Containers,
	})
}

func handleHostContainers(c *gin.Context) {
	hostID := c.Param("id")

	cli, err := getDockerClientForHost(hostID)
	if err != nil || cli == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Hote non trouve"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	var result []gin.H
	for _, ctr := range containers {
		name := ""
		if len(ctr.Names) > 0 {
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}
		result = append(result, gin.H{
			"id":      ctr.ID[:12],
			"name":    name,
			"image":   ctr.Image,
			"status":  ctr.Status,
			"state":   ctr.State,
			"project": ctr.Labels["com.docker.compose.project"],
			"service": ctr.Labels["com.docker.compose.service"],
		})
	}
	if result == nil {
		result = []gin.H{}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "host_id": hostID, "containers": result})
}

func handleHostInfo(c *gin.Context) {
	hostID := c.Param("id")

	cli, err := getDockerClientForHost(hostID)
	if err != nil || cli == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "Hote non trouve"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := cli.Info(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"host_id": hostID,
		"info": gin.H{
			"name":               info.Name,
			"os":                 info.OperatingSystem,
			"os_type":            info.OSType,
			"architecture":       info.Architecture,
			"kernel_version":     info.KernelVersion,
			"server_version":     info.ServerVersion,
			"cpus":               info.NCPU,
			"memory_bytes":       info.MemTotal,
			"containers":         info.Containers,
			"containers_running": info.ContainersRunning,
			"containers_paused":  info.ContainersPaused,
			"containers_stopped": info.ContainersStopped,
			"images":             info.Images,
			"storage_driver":     info.Driver,
			"docker_root_dir":    info.DockerRootDir,
		},
	})
}
