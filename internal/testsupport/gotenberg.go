package testsupport

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/inspecto-system/report-service/internal/config"
)

// gotenbergImage is the image of platform/deploy/compose.dev.yaml, with the same restrictions.
const gotenbergImage = "gotenberg/gotenberg:8.39.0"

// StartGotenberg starts Gotenberg, removed when the test ends.
func StartGotenberg(t *testing.T) config.Gotenberg {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: gotenbergImage,
			Cmd: []string{
				"gotenberg", "--chromium-allow-list=^file:///tmp/.*", "--chromium-disable-javascript=true",
				"--log-level=warn",
			},
			ExposedPorts: []string{"3000/tcp"},
			WaitingFor:   wait.ForHTTP("/health").WithPort("3000/tcp"),
		},
		Started: true,
	})
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	if err != nil {
		t.Fatalf("start Gotenberg: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "3000/tcp", "http")
	if err != nil {
		t.Fatalf("gotenberg endpoint: %v", err)
	}
	return config.Gotenberg{URL: endpoint}
}
