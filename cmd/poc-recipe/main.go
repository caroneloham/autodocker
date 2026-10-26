// poc-recipe writes an isolated recipe using the production generator. It never
// starts Docker or reads an existing client's credentials.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"autodocker-go/services"
	"github.com/goccy/go-yaml"
)

func main() {
	out := flag.String("out", "", "New directory for the private generated recipe")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if _, err := os.Stat(*out); !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "Output must be a new directory")
		os.Exit(2)
	}
	p := services.BuildComposePayload{Client: &services.ClientDeliveryConfig{Name: "AutoDocker · Recette POC", Description: "Plateforme de validation isolée", Accent: "#4f8cff"}, Supervision: &services.SupervisionConfig{GPU: true, DiskPath: ".", DiskWarning: 85, MinFreeGiB: 15, TemperatureWarning: 85, BackupMaxHours: 30}, AIStack: &services.AIStackConfig{Engine: "vllm", Model: "Qwen/Qwen2.5-VL-3B-Instruct", Vision: true, PrivatePlatform: true, MaxContext: 8192, Concurrency: 2, GPUMemory: 0.85}, Ingress: &services.IngressConfig{Proxy: "caddy", Domain: "localhost", Port: 19843, TLS: "internal"}, Services: []services.ServiceEntry{{Key: "vllm", Network: "app"}, {Key: "open-webui", Network: "app"}, {Key: "tei", Network: "app"}, {Key: "qdrant", Network: "app"}, {Key: "caddy", Network: "app", Image: "caddy:2-alpine"}}, Addressing: services.AddressingConfig{Networks: []services.NetworkDef{{Name: "app", CIDR: "172.31.82.0/24", Gateway: "172.31.82.1"}}}}
	if err := services.ValidateInfrastructureOptions(p); err != nil {
		panic(err)
	}
	compose := services.BuildCompose(p, "autodocker-poc-qa")
	// Test exposure is loopback-only; the generated service topology and all
	// internal application settings remain exactly those of the real generator.
	compose["services"].(services.M)["caddy"].(services.M)["ports"] = []string{"127.0.0.1:19843:443"}
	data, err := yaml.Marshal(compose)
	if err != nil {
		panic(err)
	}
	if err = os.MkdirAll(*out, 0700); err != nil {
		panic(err)
	}
	files := services.ClientDeliveryProjectFiles(compose)
	for name, text := range services.PrivateAIProjectFiles(compose) {
		files[name] = text
	}
	files["compose.yaml"] = string(data)
	for name, text := range files {
		if err = os.WriteFile(filepath.Join(*out, name), []byte(text), 0600); err != nil {
			panic(err)
		}
	}
	fmt.Println("Private recipe generated; keep this directory private. It contains generated credentials.")
}
