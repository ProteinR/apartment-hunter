package postgres

const schemaSQL = `
create extension if not exists postgis;

create table if not exists listing_evaluations (
	id bigserial primary key,
	listing_id text not null unique,
	profile_id text,
	telegram_chat_id bigint,
	source jsonb not null default '{}'::jsonb,
	channel_id text,
	channel_name text,
	message_id integer,
	bot_message_id integer,
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

alter table listing_evaluations
	add column if not exists bot_message_id integer;

create index if not exists listing_evaluations_bot_message_idx
	on listing_evaluations (telegram_chat_id, bot_message_id);

alter table listing_evaluations
	add column if not exists ai_provider text,
	add column if not exists ai_model text,
	add column if not exists input_tokens integer,
	add column if not exists cached_input_tokens integer,
	add column if not exists output_tokens integer,
	add column if not exists reasoning_tokens integer,
	add column if not exists total_tokens integer,
	add column if not exists input_price_per_million_usd numeric(12, 6),
	add column if not exists cached_input_price_per_million_usd numeric(12, 6),
	add column if not exists output_price_per_million_usd numeric(12, 6),
	add column if not exists cost_usd numeric(14, 8);

update listing_evaluations
set input_tokens = coalesce(input_tokens, 0),
	cached_input_tokens = coalesce(cached_input_tokens, 0),
	output_tokens = coalesce(output_tokens, 0),
	reasoning_tokens = coalesce(reasoning_tokens, 0),
	total_tokens = coalesce(total_tokens, 0),
	input_price_per_million_usd = coalesce(input_price_per_million_usd, 0),
	cached_input_price_per_million_usd = coalesce(cached_input_price_per_million_usd, 0),
	output_price_per_million_usd = coalesce(output_price_per_million_usd, 0),
	cost_usd = coalesce(cost_usd, 0)
where input_tokens is null
	or cached_input_tokens is null
	or output_tokens is null
	or reasoning_tokens is null
	or total_tokens is null
	or input_price_per_million_usd is null
	or cached_input_price_per_million_usd is null
	or output_price_per_million_usd is null
	or cost_usd is null;

create table if not exists profiles (
	id text primary key,
	telegram_chat_id bigint unique,
	preferences jsonb,
	debug_mode boolean not null default true,
	mcp_mode boolean not null default false,
	filter_disabled boolean not null default false,
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now()
);

alter table profiles
	add column if not exists debug_mode boolean not null default true,
	add column if not exists mcp_mode boolean not null default false,
	add column if not exists filter_disabled boolean not null default false;

create table if not exists user_search_profiles (
	id bigserial primary key,
	user_id text not null,
	raw_query text not null,
	normalized_query text not null,
	preferences_json jsonb not null,
	search_area geometry(MultiPolygon, 4326),
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now()
);

create index if not exists user_search_profiles_user_created_idx
	on user_search_profiles (user_id, created_at desc);

create index if not exists user_search_profiles_search_area_gix
	on user_search_profiles using gist (search_area);

create table if not exists listings (
	id text primary key,
	latitude double precision,
	longitude double precision,
	location geometry(Point, 4326),
	payload jsonb not null default '{}'::jsonb,
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now()
);

create index if not exists listings_location_gix
	on listings using gist (location);

create or replace function listings_set_location()
returns trigger as $$
begin
	if new.longitude is not null and new.latitude is not null then
		new.location = ST_SetSRID(ST_MakePoint(new.longitude, new.latitude), 4326);
	end if;
	return new;
end;
$$ language plpgsql;

drop trigger if exists listings_set_location_trigger on listings;
create trigger listings_set_location_trigger
	before insert or update of latitude, longitude on listings
	for each row execute function listings_set_location();

create table if not exists profile_channels (
	profile_id text not null references profiles(id) on delete cascade,
	channel_id text not null,
	username text,
	title text,
	cursor jsonb not null default '{}'::jsonb,
	limit_count integer not null default 50,
	created_at timestamptz not null default now(),
	updated_at timestamptz not null default now(),
	primary key (profile_id, channel_id)
);

create index if not exists profile_channels_username_idx
	on profile_channels (username);

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

create table if not exists listing_feedbacks (
	id bigserial primary key,
	evaluation_id bigint references listing_evaluations(id) on delete set null,
	listing_id text,
	profile_id text,
	telegram_chat_id bigint not null,
	feedback_message_id integer not null,
	replied_message_id integer not null,
	feedback_text text not null,
	status text not null default 'open',
	resolved_commit text,
	resolution_note text,
	created_at timestamptz not null default now(),
	resolved_at timestamptz,
	updated_at timestamptz not null default now(),
	unique (telegram_chat_id, feedback_message_id)
);

create index if not exists listing_feedbacks_status_created_idx
	on listing_feedbacks (status, created_at asc);

create index if not exists listing_feedbacks_listing_idx
	on listing_feedbacks (listing_id);
`
