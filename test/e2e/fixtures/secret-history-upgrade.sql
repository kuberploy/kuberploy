-- Metadata-only representative history written under the published baseline.
INSERT INTO users(id,display_name,role,issuer,subject) VALUES
    ('70000000-0000-4000-8000-000000000001','Upgrade fixture','platform-admin','test','upgrade-fixture');
INSERT INTO projects(id,name,slug) VALUES
    ('70000000-0000-4000-8000-000000000002','Upgrade fixture','upgrade-fixture');
INSERT INTO environments(id,project_id,name,slug,namespace,argo_project) VALUES
    ('70000000-0000-4000-8000-000000000003','70000000-0000-4000-8000-000000000002',
     'Upgrade fixture','upgrade','kp-upgrade-fixture','kp-upgrade-fixture');
INSERT INTO applications(id,project_id,name,slug) VALUES
    ('70000000-0000-4000-8000-000000000004','70000000-0000-4000-8000-000000000002','Upgrade App','upgrade-app');
INSERT INTO secret_bindings(id,project_id,environment_id,application_id,target_namespace,
    name,provider,state,active_version,created_by,created_at,updated_at,delete_started_at,deleted_at) VALUES
    ('70000000-0000-4000-8000-000000000005','70000000-0000-4000-8000-000000000002',
     '70000000-0000-4000-8000-000000000003','70000000-0000-4000-8000-000000000004',
     'kp-upgrade-fixture','deleted-secret','sealed-secrets','deleted',0,
     '70000000-0000-4000-8000-000000000001',now(),now(),now(),now());
INSERT INTO secret_binding_versions(id,binding_id,version_number,provider,state,
    fingerprint_key_id,content_fingerprint,staged_at,created_at,updated_at) VALUES
    ('70000000-0000-4000-8000-000000000006','70000000-0000-4000-8000-000000000005',
     1,'sealed-secrets','deleted','upgrade-fixture',decode(repeat('00',32),'hex'),now(),now(),now());
INSERT INTO secret_binding_deliveries(version_id,binding_id,ordinal,source_key,kind,environment_name) VALUES
    ('70000000-0000-4000-8000-000000000006','70000000-0000-4000-8000-000000000005',
     0,'value','environment','UPGRADE_FIXTURE');
INSERT INTO secret_binding_events(id,binding_id,version_id,actor_id,kind,request_id,occurred_at) VALUES
    ('70000000-0000-4000-8000-000000000007','70000000-0000-4000-8000-000000000005',
     '70000000-0000-4000-8000-000000000006','70000000-0000-4000-8000-000000000001',
     'binding-deleted','upgrade-fixture',now());
INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,
    request_fingerprint,secret_binding_id,secret_version_id) VALUES
    ('70000000-0000-4000-8000-000000000001','secret-binding','create',
     '70000000-0000-4000-8000-000000000004','upgrade-fixture-receipt',decode(repeat('11',32),'hex'),
     '70000000-0000-4000-8000-000000000005','70000000-0000-4000-8000-000000000006');
