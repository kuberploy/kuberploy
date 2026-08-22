package helmapps

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/id"
)

type nestedProtectedResolverBeginner struct {
	tx pgx.Tx
}

func (b nestedProtectedResolverBeginner) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if options.IsoLevel != pgx.Serializable || options.AccessMode != pgx.ReadOnly {
		return nil, errors.New("protected resolver did not request a serializable read-only snapshot")
	}
	return b.tx.Begin(ctx)
}

func TestPostgresProtectedBindingResolverRejectsStaleOrSubstitutedSnapshots(t *testing.T) {
	databaseURL := os.Getenv("KUBERPLOY_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set KUBERPLOY_TEST_DATABASE_URL for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = testdb.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	outer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outer.Rollback(context.Background()) })

	fixture := newHelmReleasePGFixture()
	setupHelmReleasePGFixture(t, ctx, outer, fixture)
	target := ReleaseTarget{ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		ApplicationID: fixture.applicationID}
	config := ProtectedBindingResolverConfig{PlatformBindingID: fixture.platformBindingID,
		ClusterID: fixture.clusterID}
	resolver := &PostgresProtectedBindingResolver{begin: nestedProtectedResolverBeginner{tx: outer}, config: config}
	resolved, err := resolver.ResolveProtectedBinding(ctx, target)
	if err != nil || resolved.Validate() != nil || resolved.PlatformBindingID != fixture.platformBindingID ||
		resolved.EnvironmentBindingID != fixture.environmentBindingID || resolved.ClusterID != fixture.clusterID ||
		resolved.EnvironmentRevision != fixture.environmentHead || resolved.EnvironmentGeneration != 1 ||
		resolved.PlannedBaseRevision != fixture.platformHead {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	// Activated generations are immutable history. A binding may therefore have
	// many rows whose state is active; only its exact projection_generation is
	// current authority.
	history, err := outer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	historyHead := strings.Repeat("4", 40)
	if _, err = history.Exec(ctx, `INSERT INTO git_projection_generations(
		binding_id,generation,head_revision,parser_version,state,started_at,activated_at
	) SELECT binding_id,2,$2,parser_version,'active',$3,$3
		FROM git_projection_generations WHERE binding_id=$1 AND generation=1`,
		fixture.environmentBindingID, historyHead, fixture.now.Add(time.Second)); err != nil {
		_ = history.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err = history.Exec(ctx, `UPDATE git_repository_bindings SET
		target_head_revision=$2,indexed_revision=$2,projection_generation=2,
		target_head_observed_at=$3,indexed_at=$3,updated_at=$3 WHERE id=$1`,
		fixture.environmentBindingID, historyHead, fixture.now.Add(time.Second)); err != nil {
		_ = history.Rollback(ctx)
		t.Fatal(err)
	}
	historyResolver := &PostgresProtectedBindingResolver{
		begin: nestedProtectedResolverBeginner{tx: history}, config: config,
	}
	historyResolved, historyErr := historyResolver.ResolveProtectedBinding(ctx, target)
	if historyErr != nil || historyResolved.EnvironmentGeneration != 2 ||
		historyResolved.EnvironmentRevision != historyHead {
		_ = history.Rollback(ctx)
		t.Fatalf("historical active generation selected: resolved=%#v err=%v", historyResolved, historyErr)
	}
	if err = history.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wantCatalog, err := protectedCatalogDigest([]ApprovalDocument{releaseApprovalDocumentFromPGFixture(t, fixture)})
	if err != nil || resolved.CatalogDigest != wantCatalog {
		t.Fatalf("catalog=%s want=%s err=%v", resolved.CatalogDigest, wantCatalog, err)
	}
	// Provider/repository/ref identity is immutable in PostgreSQL. The resolver
	// revalidates the resulting binding, while the database rejects attempts to
	// substitute those authority coordinates before another snapshot can see it.
	expectPGCheck(t, ctx, outer, func(nested pgx.Tx) error {
		_, mutationErr := nested.Exec(ctx, `UPDATE git_repository_bindings
			SET target_ref='refs/heads/substituted' WHERE id=$1`, fixture.environmentBindingID)
		return mutationErr
	})
	expectPGCheck(t, ctx, outer, func(nested pgx.Tx) error {
		_, mutationErr := nested.Exec(ctx, `UPDATE git_repository_bindings
			SET repository_id=999 WHERE id=$1`, fixture.environmentBindingID)
		return mutationErr
	})

	applicationSubstitutionID := id.New()
	tests := []struct {
		name   string
		target ReleaseTarget
		mutate func(pgx.Tx) error
	}{
		{name: "platform unready", target: target, mutate: func(tx pgx.Tx) error {
			_, mutationErr := tx.Exec(ctx, `UPDATE git_repository_bindings SET
				state='waiting-for-git',target_head_revision=NULL,target_head_observed_at=NULL
				WHERE id=$1`, fixture.platformBindingID)
			return mutationErr
		}},
		{name: "environment index lag", target: target, mutate: func(tx pgx.Tx) error {
			_, mutationErr := tx.Exec(ctx, `UPDATE git_repository_bindings SET
				state='indexing',target_head_revision=$2 WHERE id=$1`, fixture.environmentBindingID,
				strings.Repeat("2", 40))
			return mutationErr
		}},
		{name: "active generation substituted", target: target, mutate: func(tx pgx.Tx) error {
			_, mutationErr := tx.Exec(ctx, `UPDATE git_projection_generations
				SET head_revision=$2 WHERE binding_id=$1 AND generation=1`, fixture.environmentBindingID,
				strings.Repeat("3", 40))
			return mutationErr
		}},
		{name: "invalid projected document", target: target, mutate: func(tx pgx.Tx) error {
			_, mutationErr := tx.Exec(ctx, `INSERT INTO git_projected_documents(
				binding_id,generation,path,application_id,source_revision,config_revision,
				blob_id,content_sha256,raw,parsed,valid,diagnostics,schema_version,
				parser_version,indexed_at
			) VALUES($1,1,'applications/sample.yaml',$2,$3,$3,$3,$4,$5,NULL,false,
				'[{"code":"invalid"}]'::jsonb,'appconfig.v1alpha1','gitprojection.v1',$6)`,
				fixture.environmentBindingID, fixture.applicationID, fixture.environmentHead,
				helmPGDigest([]byte("invalid")), []byte("invalid: true\n"), fixture.now)
			return mutationErr
		}},
		{name: "environment unready", target: target, mutate: func(tx pgx.Tx) error {
			_, mutationErr := tx.Exec(ctx, `UPDATE git_repository_bindings SET
				state='waiting-for-git',target_head_revision=NULL,target_head_observed_at=NULL
				WHERE id=$1`, fixture.environmentBindingID)
			return mutationErr
		}},
		{name: "application project substituted", target: ReleaseTarget{ProjectID: fixture.projectID,
			EnvironmentID: fixture.environmentID, ApplicationID: applicationSubstitutionID}, mutate: func(tx pgx.Tx) error {
			otherProjectID := id.New()
			if _, mutationErr := tx.Exec(ctx, `INSERT INTO projects(id,name,slug,created_at)
				VALUES($1,'Other Project',$2,$3)`, otherProjectID, "other-"+otherProjectID, fixture.now); mutationErr != nil {
				return mutationErr
			}
			_, mutationErr := tx.Exec(ctx, `INSERT INTO applications(id,project_id,name,slug,created_at)
				VALUES($1,$2,'Other Application',$3,$4)`, applicationSubstitutionID, otherProjectID,
				"other-"+otherProjectID, fixture.now)
			return mutationErr
		}},
		{name: "approval identity substituted", target: target, mutate: func(tx pgx.Tx) error {
			return insertSubstitutedApprovalDocument(ctx, tx, fixture)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutation, beginErr := outer.Begin(ctx)
			if beginErr != nil {
				t.Fatal(beginErr)
			}
			defer mutation.Rollback(ctx) //nolint:errcheck
			if mutationErr := test.mutate(mutation); mutationErr != nil {
				t.Fatal(mutationErr)
			}
			candidate := &PostgresProtectedBindingResolver{
				begin: nestedProtectedResolverBeginner{tx: mutation}, config: config,
			}
			if got, resolveErr := candidate.ResolveProtectedBinding(ctx, test.target); resolveErr == nil ||
				got != (ProtectedBindingSnapshot{}) {
				t.Fatalf("substituted authority resolved=%#v err=%v", got, resolveErr)
			}
		})
	}
}

func releaseApprovalDocumentFromPGFixture(t *testing.T, fixture helmReleasePGFixture) ApprovalDocument {
	t.Helper()
	return ApprovalDocument{Approval: Approval{ApprovalKey: ApprovalKey{ID: fixture.approvalID, Revision: 1},
		OCIRepository: fixture.ociRepository, ChartVersion: "1.2.3",
		ManifestDigest: helmPGDigest([]byte("chart-manifest")), PackageDigest: helmPGDigest([]byte("chart-package")),
		ValuesSchemaDigest: fixture.schemaDigest, RendererImage: RendererImage, RendererVersion: HelmVersion,
		PolicyVersion: PolicyVersion, CreatedBy: fixture.userID, IdempotencyKey: "approval-" + fixture.approvalID,
		CreatedAt: fixture.now}, ValuesSchemaJSON: fixture.schema, DefaultValuesYAML: fixture.values,
		PackageBytes: []byte("chart-package"), DocumentsDigest: fixture.documentsDigest, CreatedAt: fixture.now}
}

func insertSubstitutedApprovalDocument(ctx context.Context, tx pgx.Tx, fixture helmReleasePGFixture) error {
	approvalID := id.New()
	key := ApprovalKey{ID: approvalID, Revision: 1}
	documentsDigest, err := approvalDocumentsDigest(key, fixture.schema, fixture.values)
	if err != nil {
		return err
	}
	manifestDigest, packageDigest := helmPGDigest([]byte("other-manifest")), helmPGDigest([]byte("other-package"))
	repository := "oci://registry.example.com/platform/substituted-" + strings.ReplaceAll(approvalID, "-", "")
	chartName := "substituted-" + strings.ReplaceAll(approvalID, "-", "")
	if _, err = tx.Exec(ctx, `INSERT INTO helm_chart_approvals(
		approval_id,revision,source_kind,source_json,chart_name,oci_repository,chart_version,manifest_digest,package_digest,
		values_schema_digest,renderer_image,renderer_version,policy_version,identity_digest,
		created_by,idempotency_key,created_at
	) VALUES($1,1,'oci',jsonb_build_object('kind','oci','oci',jsonb_build_object(
		'repository',$2::text,'version','2.0.0','digest',$4::text)),$3,$2,'2.0.0',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, approvalID,
		repository, chartName,
		manifestDigest, packageDigest, fixture.schemaDigest, RendererImage, HelmVersion, PolicyVersion,
		helmPGDigest([]byte("substituted-identity")), fixture.userID, "approval-"+approvalID, fixture.now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO helm_chart_approval_documents(
		approval_id,approval_revision,values_schema_json,default_values_yaml,package_bytes,
		values_schema_digest,documents_digest,created_at
	) VALUES($1,1,$2,$3,$4,$5,$6,$7)`, approvalID, fixture.schema, fixture.values,
		[]byte("other-package"), fixture.schemaDigest, documentsDigest, fixture.now)
	return err
}
