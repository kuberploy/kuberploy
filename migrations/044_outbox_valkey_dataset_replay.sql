CREATE TABLE IF NOT EXISTS outbox_valkey_dataset (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    dataset_id uuid NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now()
);
