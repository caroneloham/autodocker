package services

import (
	"context"
	"fmt"
	"strings"
)

func GenerateLMStudioConfig(prompt string) (map[string]interface{}, error) {
	return generateInfrastructurePayload(prompt, "lmstudio")
}

func generateInfrastructurePayload(prompt, provider string) (map[string]interface{}, error) {
	result, err := generateInfrastructureWithProvider(context.Background(), prompt, infrastructureProviderForName(provider))
	if err != nil {
		return nil, err
	}
	if len(result.Questions) > 0 {
		return nil, fmt.Errorf("précision nécessaire : %s", strings.Join(result.Questions, " "))
	}
	return result.Payload, nil
}
