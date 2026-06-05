package postgres

const schemaSQL = `
create table if not exists listing_evaluations (
	id bigserial primary key,
	listing_id text not null unique,
	profile_id text,
	telegram_chat_id bigint,
	source jsonb not null default '{}'::jsonb,
	channel_id text,
	channel_name text,
	message_id integer,
	message_link text,
	listing jsonb not null,
	score jsonb,
	insights jsonb,
	ai_usage jsonb,
	duplicate jsonb,
	report text,
	status text not null,
	failure_reason text,
	ai_provider text,
	ai_model text,
	input_tokens integer,
	cached_input_tokens integer,
	output_tokens integer,
	reasoning_tokens integer,
	total_tokens integer,
	input_price_per_million_usd numeric(12, 6),
	cached_input_price_per_million_usd numeric(12, 6),
	output_price_per_million_usd numeric(12, 6),
	cost_usd numeric(14, 8),
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now()
);

create index if not exists listing_evaluations_profile_created_idx
	on listing_evaluations (profile_id, created_at desc);

create index if not exists listing_evaluations_status_idx
	on listing_evaluations (status);

create index if not exists listing_evaluations_message_idx
	on listing_evaluations (channel_id, message_id);

alter table listing_evaluations
	add column if not exists report text;

create table if not exists listing_images (
	id bigserial primary key,
	listing_id text not null,
	source_image_url text not null,
	hashes jsonb not null,
	sha256 text,
	phash_full text,
	phash_center_80 text,
	phash_center_60 text,
	dhash_full text,
	created_at timestamptz not null default now(),
	unique (listing_id, source_image_url)
);

create index if not exists listing_images_listing_idx
	on listing_images (listing_id);

create index if not exists listing_images_sha256_idx
	on listing_images (sha256);

create table if not exists listing_duplicates (
	id bigserial primary key,
	listing_id text not null unique,
	canonical_listing_id text not null,
	matched_images integer not null default 0,
	strong_matches integer not null default 0,
	weak_matches integer not null default 0,
	confidence double precision not null,
	reason text not null,
	created_at timestamptz not null default now()
);
`
