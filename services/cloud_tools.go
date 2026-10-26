package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrCloudToolBusy = errors.New("les outils IA sont occupés ; réessayez dans quelques instants")
var ErrCloudToolInput = errors.New("paramètres d’outil invalides")
var cloudToolSlots = make(chan struct{}, 2)
var cloudToolRequest = cloudToolJSON

type CloudSearchItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Excerpt string `json:"excerpt,omitempty"`
}

type CloudSearchResult struct {
	Provider string            `json:"provider"`
	Results  []CloudSearchItem `json:"results"`
}

type CloudVisionResult struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Answer   string `json:"answer"`
}

func validateCloudToolText(text string, max int) bool {
	if utf8.RuneCountInString(text) < 3 || utf8.RuneCountInString(text) > max {
		return false
	}
	// Never forward a configured key pasted into a question to another service.
	for _, spec := range cloudSpecs {
		for _, key := range infrastructureCredentials(spec.Name) {
			if key != "" && strings.Contains(text, key) {
				return false
			}
		}
	}
	return true
}

func cloudToolProviders(role, selected string) ([]infrastructureProvider, CloudConfig, error) {
	config, err := readCloudConfig()
	if err != nil {
		return nil, config, err
	}
	order := config.SearchOrder
	if role == "vision" {
		order = config.VisionOrder
	}
	if selected != "" {
		order = []string{selected}
	} else if order == nil {
		if role == "search" {
			order = []string{"tavily", "exa", "firecrawl", "parallel", "serpsearch"}
		} else {
			order = []string{"ollama-cloud", "moondream"}
		}
	}
	providers := []infrastructureProvider{}
	for _, name := range order {
		spec, known := cloudSpecFor(name)
		if !known || !cloudHasRole(spec, role) {
			return nil, config, fmt.Errorf("%w : fournisseur incompatible", ErrCloudToolInput)
		}
		if cloudEnabled(spec, config) {
			providers = append(providers, infrastructureProvider{name: name})
		}
	}
	if len(providers) == 0 {
		return nil, config, fmt.Errorf("aucune API active pour cet outil")
	}
	return providers, config, nil
}

func cloudToolJSON(ctx context.Context, method, endpoint, key, header string, body interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("requête outil invalide")
	}
	var reader io.Reader = bytes.NewReader(data)
	if method == http.MethodGet {
		reader = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("requête outil invalide")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AutoDocker/1.0")
	if header == "Authorization" {
		key = "Bearer " + key
	}
	req.Header.Set(header, key)
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &infrastructureProviderError{message: "l’API de l’outil ne répond pas"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &infrastructureProviderError{status: response.StatusCode, retryAfter: infrastructureRetryAfter(response.Header.Get("Retry-After"), time.Now()), message: fmt.Sprintf("l’API de l’outil a refusé la requête (HTTP %d)", response.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	var result map[string]interface{}
	if err != nil || len(raw) > 2<<20 || json.Unmarshal(raw, &result) != nil {
		return nil, &infrastructureProposalError{message: "réponse de l’outil inexploitable"}
	}
	return result, nil
}

func CloudSearch(ctx context.Context, query, selected string) (CloudSearchResult, error) {
	query = strings.TrimSpace(query)
	if !validateCloudToolText(query, 500) {
		return CloudSearchResult{}, fmt.Errorf("%w : recherche de 3 à 500 caractères, sans clé API", ErrCloudToolInput)
	}
	providers, config, err := cloudToolProviders("search", selected)
	if err != nil {
		return CloudSearchResult{}, err
	}
	select {
	case cloudToolSlots <- struct{}{}:
		defer func() { <-cloudToolSlots }()
	default:
		return CloudSearchResult{}, ErrCloudToolBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := infrastructureAutoRouter.generate(ctx, "", providers, func(ctx context.Context, _ string, p infrastructureProvider) (*MagicAIResult, error) {
		spec, _ := cloudSpecFor(p.name)
		base, _, err := cloudSettings(spec, config)
		if err != nil {
			return nil, err
		}
		method, endpoint, header := http.MethodPost, base+"/search", "Authorization"
		body := map[string]interface{}{"query": query}
		switch p.name {
		case "tavily":
			body["max_results"], body["search_depth"] = 3, "basic"
		case "exa":
			header, body["numResults"] = "x-api-key", 3
		case "firecrawl":
			body["limit"] = 3
		case "parallel":
			header = "x-api-key"
			body = map[string]interface{}{"search_queries": []string{query}, "mode": "fast", "max_chars_total": 1800}
		case "serpsearch":
			method = http.MethodGet
			endpoint += "?" + url.Values{"query": {query}, "num": {"3"}}.Encode()
		}
		data, err := cloudToolRequest(ctx, method, endpoint, nextInfrastructureCredential(p.name), header, body)
		if err != nil {
			return nil, err
		}
		raw := data["results"]
		if raw == nil {
			raw = data["organic_results"]
		}
		if raw == nil {
			raw = data["data"]
		}
		if nested, ok := raw.(map[string]interface{}); ok {
			raw = nested["web"]
		}
		items, _ := raw.([]interface{})
		links := []CloudSearchItem{}
		for _, item := range items {
			object, ok := item.(map[string]interface{})
			if !ok || len(links) >= 3 {
				continue
			}
			href, _ := object["url"].(string)
			if href == "" {
				href, _ = object["link"].(string)
			}
			parsed, err := url.Parse(href)
			if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || len(href) > 2000 || redactCloudSecrets(href) != href {
				continue
			}
			title, _ := object["title"].(string)
			excerpt, _ := object["content"].(string)
			for _, field := range []string{"snippet", "description"} {
				if excerpt == "" {
					excerpt, _ = object[field].(string)
				}
			}
			if parts := toStringList(object["excerpts"]); excerpt == "" && len(parts) > 0 {
				excerpt = strings.Join(parts, " ")
			}
			links = append(links, CloudSearchItem{Title: boundedAIText(redactCloudSecrets(title), 180), URL: href, Excerpt: boundedAIText(redactCloudSecrets(excerpt), 600)})
		}
		if len(links) == 0 {
			return nil, &infrastructureProposalError{message: "aucun résultat de recherche exploitable"}
		}
		return &MagicAIResult{Payload: map[string]interface{}{"results": links}}, nil
	})
	if err != nil {
		return CloudSearchResult{}, err
	}
	links, _ := result.Payload["results"].([]CloudSearchItem)
	return CloudSearchResult{Provider: result.Source, Results: links}, nil
}

func CloudVision(ctx context.Context, imageData, question, selected string) (CloudVisionResult, error) {
	question = strings.TrimSpace(question)
	if !validateCloudToolText(question, 1000) || len(imageData) > 2<<20 {
		return CloudVisionResult{}, fmt.Errorf("%w : question ou image invalide", ErrCloudToolInput)
	}
	var encoded string
	for _, prefix := range []string{"data:image/png;base64,", "data:image/jpeg;base64,"} {
		if strings.HasPrefix(imageData, prefix) {
			encoded = strings.TrimPrefix(imageData, prefix)
		}
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return CloudVisionResult{}, fmt.Errorf("%w : image PNG/JPEG intégrée, 1 Mio maximum ; aucune URL distante", ErrCloudToolInput)
	}
	dimensions, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > 4096 || dimensions.Height > 4096 {
		return CloudVisionResult{}, fmt.Errorf("%w : dimensions d’image invalides", ErrCloudToolInput)
	}
	providers, config, err := cloudToolProviders("vision", selected)
	if err != nil {
		return CloudVisionResult{}, err
	}
	select {
	case cloudToolSlots <- struct{}{}:
		defer func() { <-cloudToolSlots }()
	default:
		return CloudVisionResult{}, ErrCloudToolBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := infrastructureAutoRouter.generate(ctx, "", providers, func(ctx context.Context, _ string, p infrastructureProvider) (*MagicAIResult, error) {
		spec, _ := cloudSpecFor(p.name)
		base, model, err := cloudSettings(spec, config)
		if err != nil {
			return nil, err
		}
		endpoint, header := base+"/chat/completions", "Authorization"
		var body interface{} = map[string]interface{}{"model": model, "messages": []interface{}{map[string]interface{}{"role": "user", "content": []interface{}{map[string]string{"type": "text", "text": question}, map[string]interface{}{"type": "image_url", "image_url": map[string]string{"url": imageData}}}}}, "max_tokens": 384, "stream": false}
		if p.name == "moondream" {
			endpoint, header = base+"/query", "X-Moondream-Auth"
			body = map[string]interface{}{"model": model, "image_url": imageData, "question": question, "stream": false, "settings": map[string]int{"max_tokens": 384}}
		}
		response, err := cloudToolRequest(ctx, http.MethodPost, endpoint, nextInfrastructureCredential(p.name), header, body)
		if err != nil {
			return nil, err
		}
		answer, _ := response["answer"].(string)
		if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
			choice, _ := choices[0].(map[string]interface{})
			message, _ := choice["message"].(map[string]interface{})
			answer, _ = message["content"].(string)
		}
		if strings.TrimSpace(answer) == "" {
			return nil, &infrastructureProposalError{message: "aucune réponse vision exploitable"}
		}
		return &MagicAIResult{Model: model, Payload: map[string]interface{}{"answer": boundedAIText(redactCloudSecrets(answer), 3000)}}, nil
	})
	if err != nil {
		return CloudVisionResult{}, err
	}
	answer, _ := result.Payload["answer"].(string)
	return CloudVisionResult{Provider: result.Source, Model: result.Model, Answer: answer}, nil
}
