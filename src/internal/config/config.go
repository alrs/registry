package config

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-xray-sdk-go/xray"
	gogithub "github.com/google/go-github/v54/github"
	"github.com/opentffoundation/registry/internal/github"
	"github.com/opentffoundation/registry/internal/providers/providercache"
	"github.com/opentffoundation/registry/internal/secrets"
	"github.com/shurcooL/githubv4"
	"os"
)

type ConfigBuilder struct {
	IncludeProviderRedirects bool
}

func NewConfigBuilder(options ...func(*ConfigBuilder)) *ConfigBuilder {
	configBuilder := &ConfigBuilder{}
	for _, option := range options {
		option(configBuilder)
	}
	return configBuilder
}

func WithProviderRedirects() func(*ConfigBuilder) {
	return func(builder *ConfigBuilder) {
		builder.IncludeProviderRedirects = true
	}
}

type Config struct {
	ManagedGithubClient *gogithub.Client
	RawGithubv4Client   *githubv4.Client

	LambdaClient         *lambda.Client
	ProviderVersionCache *providercache.Handler
	SecretsHandler       *secrets.Handler

	ProviderRedirects map[string]string
}

// BuildConfig will build a configuration object for the application. This
// includes loading secrets from AWS Secrets Manager, and configuring the
// AWS SDK.
func (c ConfigBuilder) BuildConfig(ctx context.Context, xraySegmentName string) (config *Config, err error) {
	if err = xray.Configure(xray.Config{ServiceVersion: "1.2.3"}); err != nil {
		err = fmt.Errorf("could not configure X-Ray: %w", err)
		return
	}

	// At this point we're not part of a Lambda request execution, so let's
	// explicitly create a segment to represent the configuration process.
	ctx, segment := xray.BeginSegment(ctx, xraySegmentName)
	defer func() { segment.Close(err) }()

	var awsConfig aws.Config
	awsConfig, err = awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(os.Getenv("AWS_REGION")))
	if err != nil {
		err = fmt.Errorf("could not load AWS configuration: %w", err)
		return
	}

	secretsHandler := secrets.NewHandler(awsConfig)

	githubAPIToken, err := secretsHandler.GetSecretValueFromEnvReference(ctx, "GITHUB_TOKEN_SECRET_ASM_NAME")
	if err != nil {
		err = fmt.Errorf("could not get GitHub API token: %w", err)
		return
	}

	var tableName string
	tableName = os.Getenv("PROVIDER_VERSIONS_TABLE_NAME")
	if tableName == "" {
		err = fmt.Errorf("PROVIDER_VERSIONS_TABLE_NAME environment variable not set")
		return
	}

	providerRedirects := make(map[string]string)
	if c.IncludeProviderRedirects {
		if redirectsJSON, ok := os.LookupEnv("PROVIDER_NAMESPACE_REDIRECTS"); ok {
			if err := json.Unmarshal([]byte(redirectsJSON), &providerRedirects); err != nil {
				panic(fmt.Errorf("could not parse PROVIDER_NAMESPACE_REDIRECTS: %w", err))
			}
		}
	}

	config = &Config{
		ManagedGithubClient: github.NewManagedGithubClient(githubAPIToken),
		RawGithubv4Client:   github.NewRawGithubv4Client(githubAPIToken),

		SecretsHandler:       secretsHandler,
		ProviderVersionCache: providercache.NewHandler(awsConfig, tableName),
		LambdaClient:         lambda.NewFromConfig(awsConfig),

		ProviderRedirects: providerRedirects,
	}
	return
}

// EffectiveProviderNamespace will map namespaces for providers in situations
// where the author (owner of the namespace) does not release artifacts as
// GitHub Releases.
func (c Config) EffectiveProviderNamespace(namespace string) string {
	if redirect, ok := c.ProviderRedirects[namespace]; ok {
		return redirect
	}

	return namespace
}
