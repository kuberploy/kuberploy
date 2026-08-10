package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type datasetRecordingPublisher struct {
	datasetID string
	messages  []domain.WorkMessage
}

func (p *datasetRecordingPublisher) DatasetIdentity(context.Context) (string, error) {
	return p.datasetID, nil
}

func (p *datasetRecordingPublisher) Publish(_ context.Context, message domain.WorkMessage) error {
	p.messages = append(p.messages, message)
	return nil
}

func TestRelayReconstructsNonTerminalOutboxOncePerDataset(t *testing.T) {
	ctx := t.Context()
	store := memory.New()
	admin := domain.User{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Login: "Admin", Role: "platform-admin", Issuer: "test", Subject: "admin", GrantRevision: 1, CreatedAt: time.Now()}
	if err := store.BootstrapAdmin(ctx, admin, strings.Repeat("h", 64), []byte("01234567890123456789012345678901"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, admin.ID, "project", "project", domain.CreateProject{Name: "Demo", Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := store.CreateEnvironment(ctx, admin.ID, "environment", "environment", domain.CreateEnvironment{ProjectID: project.Value.ID, Name: "Dev", Slug: "dev", Namespace: "kp-demo-dev", ArgoProject: "kp-demo"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := store.CreateApplication(ctx, admin.ID, "application", "application", domain.CreateApplication{ProjectID: project.Value.ID, Name: "Hello", Slug: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, operation, err := store.CreateDeployment(ctx, admin.ID, "deployment", "deployment", "request", domain.CreateDeployment{
		EnvironmentID: environment.Value.ID, ApplicationID: application.Value.ID,
		Image: "registry.test/hello@sha256:" + strings.Repeat("b", 64), Replicas: 1, Port: 8080,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &datasetRecordingPublisher{datasetID: "11111111-1111-4111-8111-111111111111"}
	relay := &Relay{Store: store, Publisher: publisher}
	if count, replayed, runErr := relay.RunOnceWithReplay(ctx); runErr != nil || count != 1 || replayed != 0 || len(publisher.messages) != 1 || publisher.messages[0].OperationID != operation.ID {
		t.Fatalf("first dataset count=%d replayed=%d messages=%#v err=%v", count, replayed, publisher.messages, runErr)
	}
	if count, runErr := relay.RunOnce(ctx); runErr != nil || count != 0 || len(publisher.messages) != 1 {
		t.Fatalf("stable dataset count=%d messages=%d err=%v", count, len(publisher.messages), runErr)
	}
	publisher.datasetID = "22222222-2222-4222-8222-222222222222"
	if count, replayed, runErr := relay.RunOnceWithReplay(ctx); runErr != nil || count != 1 || replayed != 1 || len(publisher.messages) != 2 || publisher.messages[1].OperationID != operation.ID {
		t.Fatalf("replacement dataset count=%d replayed=%d messages=%#v err=%v", count, replayed, publisher.messages, runErr)
	}
	if count, runErr := relay.RunOnce(ctx); runErr != nil || count != 0 || len(publisher.messages) != 2 {
		t.Fatalf("reconciled dataset count=%d messages=%d err=%v", count, len(publisher.messages), runErr)
	}
}
