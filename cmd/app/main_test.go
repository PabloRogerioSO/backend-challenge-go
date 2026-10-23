//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testPortHTTP uses a port outside the range 8080 to avoid clashing with a
// running docker compose environment.
const testPortHTTP = "18080"

func TestFx_StartsAndStops(t *testing.T) {
	ctx := context.Background()

	// 1. Start PostgreSQL
	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:16",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "test",
				"POSTGRES_PASSWORD": "test",
				"POSTGRES_DB":       "wallet_test",
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pg.Terminate(context.Background()) }()
	pgHost, _ := pg.Host(ctx)
	pgPort, _ := pg.MappedPort(ctx, "5432/tcp")
	pgDSN := fmt.Sprintf("postgres://test:test@%s:%s/wallet_test?sslmode=disable", pgHost, pgPort.Port())

	// 2. Start Keycloak
	realmPath, err := filepath.Abs(filepath.Join("..", "..", "deploy", "keycloak", "realm-export.json"))
	if err != nil {
		t.Fatalf("resolve realm path: %v", err)
	}
	kc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "quay.io/keycloak/keycloak:26.0",
			ExposedPorts: []string{"8080/tcp", "9000/tcp"},
			Cmd:          []string{"start-dev", "--import-realm"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
				"KC_HEALTH_ENABLED":           "true",
				"KC_HOSTNAME_STRICT":          "false",
			},
			Mounts: testcontainers.ContainerMounts{
				testcontainers.BindMount(realmPath, "/opt/keycloak/data/import/realm-export.json"),
			},
			WaitingFor: wait.ForHTTP("/health/ready").
				WithPort("9000/tcp").
				WithStartupTimeout(180 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keycloak: %v", err)
	}
	defer func() { _ = kc.Terminate(context.Background()) }()
	kcHost, _ := kc.Host(ctx)
	kcPort, _ := kc.MappedPort(ctx, "8080/tcp")
	kcBaseURL := fmt.Sprintf("http://%s:%s", kcHost, kcPort.Port())
	kcIssuer := kcBaseURL + "/realms/backend"
	kcJWKS := kcIssuer + "/protocol/openid-connect/certs"

	// 3. Start LocalStack
	ls, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "localstack/localstack:4.14.0",
			ExposedPorts: []string{"4566/tcp"},
			Env:          map[string]string{"SERVICES": "sqs"},
			WaitingFor:   wait.ForHTTP("/_localstack/health").WithPort("4566/tcp").WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start localstack: %v", err)
	}
	defer func() { _ = ls.Terminate(context.Background()) }()
	lsHost, _ := ls.Host(ctx)
	lsPort, _ := ls.MappedPort(ctx, "4566/tcp")
	lsEndpoint := fmt.Sprintf("http://%s:%s", lsHost, lsPort.Port())

	// 4. Create the two FIFO queues the app expects.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	sqsClient := awssqs.NewFromConfig(awsCfg, func(o *awssqs.Options) {
		o.BaseEndpoint = aws.String(lsEndpoint)
	})
	inboundURL := createFIFOQueue(t, sqsClient, "wager-transactions.fifo")
	outboundURL := createFIFOQueue(t, sqsClient, "wager-transactions-out.fifo")

	// 5. Configure env vars consumed by config.Load and loadSqsConfig.
	setEnv(t, "HTTP_PORT", testPortHTTP)
	setEnv(t, "DATABASE_URL", pgDSN)
	setEnv(t, "KEYCLOAK_ISSUER_URL", kcIssuer)
	setEnv(t, "KEYCLOAK_JWKS_URL", kcJWKS)
	setEnv(t, "KEYCLOAK_AUDIENCE", "provider-service")
	setEnv(t, "SQS_ENDPOINT", lsEndpoint)
	setEnv(t, "SQS_INBOUND_QUEUE_URL", inboundURL)
	setEnv(t, "SQS_OUTBOUND_QUEUE_URL", outboundURL)
	setEnv(t, "SQS_CONSUMER_POLLERS", "2")
	setEnv(t, "REFERENCE_TTL_HOURS", "24")
	setEnv(t, "AWS_REGION", "us-east-1")
	setEnv(t, "AWS_ACCESS_KEY_ID", "test")
	setEnv(t, "AWS_SECRET_ACCESS_KEY", "test")
	setEnv(t, "AWS_EC2_METADATA_DISABLED", "true")

	// 6. Build the app graph.
	app := buildApp()
	if err := app.Err(); err != nil {
		t.Fatalf("fx graph validation: %v", err)
	}

	// 7. Start.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("fx Start: %v", err)
	}

	// 8. Probe: HTTP server responds.
	waitForHTTP(t, "http://localhost:"+testPortHTTP+"/health/live", 30*time.Second)

	// 9. Probe: readiness endpoint.
	waitForHTTP(t, "http://localhost:"+testPortHTTP+"/health/ready", 30*time.Second)

	// 10. Probe: Keycloak issues a token for provider-a.
	token := fetchProviderToken(t, kcIssuer, "provider-a")
	if token == "" {
		t.Fatal("empty access token from Keycloak")
	}

	// 11. Probe: the SQS consumer is polling (queue exists).
	if _, err := sqsClient.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(inboundURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	}); err != nil {
		t.Fatalf("GetQueueAttributes: %v", err)
	}

	// 12. Stop.
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("fx Stop: %v", err)
	}

	// 13. After Stop, the HTTP server is down.
	resp, err := http.Get("http://localhost:" + testPortHTTP + "/health/live")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("HTTP server still responding after Stop (status %d)", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- helpers

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func createFIFOQueue(t *testing.T, client *awssqs.Client, name string) string {
	t.Helper()
	out, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	return aws.ToString(out.QueueUrl)
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s: %v", url, lastErr)
}

func fetchProviderToken(t *testing.T, issuer, clientID string) string {
	t.Helper()
	endpoint := issuer + "/protocol/openid-connect/token"
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientID+"-secret")

	resp, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request failed: %d %s", resp.StatusCode, body)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return payload.AccessToken
}
