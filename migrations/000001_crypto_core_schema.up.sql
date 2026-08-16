CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE merchants (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    accept_any_payment_amount BOOLEAN NOT NULL DEFAULT FALSE,
    webhook_url VARCHAR(500),
    webhook_secret_enc TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE hd_wallets (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    coin VARCHAR(32) NOT NULL,
    network VARCHAR(32) NOT NULL,
    master_public_key TEXT NOT NULL DEFAULT '',
    encrypted_master_seed TEXT,
    hot_wallet_address TEXT NOT NULL DEFAULT '',
    derivation_account INTEGER NOT NULL DEFAULT 0,
    current_derivation_index INTEGER NOT NULL DEFAULT 0,
    required_confirmations INTEGER NOT NULL DEFAULT 1,
    finalization_confirmations INTEGER NOT NULL DEFAULT 1,
    amount_tolerance_percent NUMERIC(12, 6) NOT NULL DEFAULT 0,
    decimals INTEGER NOT NULL DEFAULT 18,
    is_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    contract_address TEXT,
    display_name VARCHAR(255),
    icon_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (coin, network)
);

CREATE TABLE hd_derivation_counters (
    address_space_key TEXT PRIMARY KEY,
    current_index INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE transactions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    merchant_id UUID NOT NULL REFERENCES merchants(id),
    wallet_id UUID,
    amount_expected NUMERIC(38, 18) NOT NULL,
    amount_base NUMERIC(38, 18) NOT NULL DEFAULT 0,
    fee_client NUMERIC(38, 18) NOT NULL DEFAULT 0,
    currency VARCHAR(32) NOT NULL DEFAULT '',
    chain_type VARCHAR(32) NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    tx_hash TEXT,
    external_ref_id VARCHAR(255),
    customer_email VARCHAR(320),
    commission_rate NUMERIC(12, 6) NOT NULL DEFAULT 0,
    commission_split_merchant_percent NUMERIC(12, 6) NOT NULL DEFAULT 100,
    expires_at TIMESTAMPTZ,
    detected_at TIMESTAMPTZ,
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (merchant_id, external_ref_id)
);

CREATE INDEX transactions_status_idx ON transactions(status);
CREATE INDEX transactions_expires_at_idx ON transactions(expires_at);

CREATE TABLE crypto_deposits (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    hd_wallet_id UUID NOT NULL REFERENCES hd_wallets(id),
    user_id UUID,
    deposit_address TEXT NOT NULL,
    payment_address TEXT,
    derivation_index INTEGER NOT NULL DEFAULT 0,
    coin VARCHAR(32) NOT NULL,
    network VARCHAR(32) NOT NULL,
    amount_expected NUMERIC(38, 18) NOT NULL DEFAULT 0,
    amount_base NUMERIC(38, 18) NOT NULL DEFAULT 0,
    fee_client NUMERIC(38, 18) NOT NULL DEFAULT 0,
    detected_amount NUMERIC(38, 18) NOT NULL DEFAULT 0,
    tx_hash TEXT,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    payment_id UUID REFERENCES transactions(id),
    confirmations INTEGER NOT NULL DEFAULT 0,
    required_confirmations INTEGER NOT NULL DEFAULT 1,
    is_late BOOLEAN NOT NULL DEFAULT FALSE,
    watch_status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    watch_expires_at TIMESTAMPTZ,
    last_inbound_at TIMESTAMPTZ,
    commission_rate NUMERIC(12, 6) NOT NULL DEFAULT 0,
    commission_split_merchant_percent NUMERIC(12, 6) NOT NULL DEFAULT 100,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX crypto_deposits_address_idx ON crypto_deposits(deposit_address);
CREATE INDEX crypto_deposits_network_status_idx ON crypto_deposits(network, status);
CREATE INDEX crypto_deposits_payment_id_idx ON crypto_deposits(payment_id);
CREATE INDEX crypto_deposits_tx_hash_idx ON crypto_deposits(tx_hash);

CREATE TABLE crypto_deposit_receipts (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    crypto_deposit_id UUID NOT NULL REFERENCES crypto_deposits(id) ON DELETE CASCADE,
    tx_hash TEXT NOT NULL,
    amount NUMERIC(38, 18) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (crypto_deposit_id, tx_hash)
);

CREATE INDEX crypto_deposit_receipts_tx_hash_idx ON crypto_deposit_receipts(tx_hash);

CREATE TABLE gas_wallets (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    chain_type VARCHAR(32) NOT NULL,
    wallet_address TEXT NOT NULL,
    private_key_enc TEXT NOT NULL,
    is_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    min_balance NUMERIC(38, 18) NOT NULL DEFAULT 0,
    max_gas_price NUMERIC(38, 18) NOT NULL DEFAULT 0,
    daily_gas_limit NUMERIC(38, 18) NOT NULL DEFAULT 0,
    max_gas_per_sweep NUMERIC(38, 18) NOT NULL DEFAULT 0,
    daily_gas_used NUMERIC(38, 18) NOT NULL DEFAULT 0,
    daily_reset_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_alert_balance NUMERIC(38, 18) NOT NULL DEFAULT 0,
    current_balance NUMERIC(38, 18),
    balance_checked_at TIMESTAMPTZ,
    balance_check_error TEXT,
    is_below_min_balance BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (chain_type)
);

CREATE TABLE sweeps (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    crypto_deposit_id UUID REFERENCES crypto_deposits(id),
    from_address TEXT NOT NULL,
    to_hot_wallet TEXT NOT NULL,
    amount NUMERIC(38, 18) NOT NULL,
    coin VARCHAR(32) NOT NULL,
    network VARCHAR(32) NOT NULL,
    is_token BOOLEAN NOT NULL DEFAULT FALSE,
    sequence INTEGER NOT NULL DEFAULT 0,
    source_receipts_count INTEGER NOT NULL DEFAULT 0,
    source_total_amount NUMERIC(38, 18) NOT NULL DEFAULT 0,
    purpose VARCHAR(64) NOT NULL DEFAULT 'DEPOSIT_FUNDS',
    origin_sweep_id UUID REFERENCES sweeps(id),
    tx_hash TEXT,
    provider_name VARCHAR(128),
    provider_order_id VARCHAR(255),
    provider_order_no VARCHAR(255),
    provider_status VARCHAR(128),
    provider_last_error TEXT,
    provider_metadata_json JSONB,
    energy_rental_expires_at TIMESTAMPTZ,
    energy_used NUMERIC(38, 18),
    energy_fee_sun NUMERIC(38, 18),
    net_usage NUMERIC(38, 18),
    net_fee_sun NUMERIC(38, 18),
    fee NUMERIC(38, 18) NOT NULL DEFAULT 0,
    status VARCHAR(64) NOT NULL DEFAULT 'PENDING',
    error_message TEXT,
    attempts INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX sweeps_status_idx ON sweeps(status);
CREATE INDEX sweeps_deposit_idx ON sweeps(crypto_deposit_id, sequence DESC);
CREATE INDEX sweeps_broadcasting_idx ON sweeps(status, updated_at);
CREATE UNIQUE INDEX sweeps_deposit_sequence_idx
    ON sweeps(crypto_deposit_id, sequence, purpose);

CREATE TABLE scanning_state (
    chain_type VARCHAR(32) PRIMARY KEY,
    last_scanned_block BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE system_settings (
    key VARCHAR(255) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE trc20_address_pool (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    address TEXT NOT NULL UNIQUE,
    hd_wallet_id UUID NOT NULL REFERENCES hd_wallets(id),
    derivation_index INTEGER NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'available',
    assigned_deposit_id UUID REFERENCES crypto_deposits(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX trc20_address_pool_status_idx ON trc20_address_pool(status, updated_at);

CREATE TABLE webhook_logs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    merchant_id UUID NOT NULL REFERENCES merchants(id),
    transaction_id UUID REFERENCES transactions(id),
    url TEXT NOT NULL,
    payload TEXT NOT NULL,
    response_code INTEGER,
    response_body TEXT,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX webhook_logs_retry_idx ON webhook_logs(status, next_retry_at);
