# Spec Delta

## Purpose

`web_search` reads its provider base URL and API key from the per-run Tavily override when the run carries one; without an override, deployment configuration applies unchanged.

## MODIFIED Requirements

### Requirement: `web_search` queries the Tavily Search API

`web_search` SHALL accept a `query` string (required, 1–400 characters) and return a compact ranked result list — for each result a title, URL, and description snippet — bounded by a configurable maximum (default 5, hard cap 10). The search provider base URL and API key SHALL come from the run's Tavily override when one is present, and otherwise from configuration; the API key SHALL be optional: with no key from either source the tool MUST operate against the provider's keyless tier (no account required), and with a configured key it MUST authenticate against the account. Provider throttling (HTTP 429), server errors (5xx), and timeouts SHALL be classified as transient (retryable); malformed or invalid queries SHALL be classified as correctable argument errors; account/plan-limit rejections SHALL NOT be retried.

#### Scenario: Search returns bounded compact results

- **WHEN** the model calls `web_search` with a valid query
- **THEN** the tool returns at most the configured maximum number of results, each carrying only title, URL, and description

#### Scenario: Run override selects provider endpoint and credential

- **WHEN** a run carries a Tavily override and the model calls `web_search`
- **THEN** the search request goes to the override's base URL with the override's credential, and the deployment-configured values are not consulted

#### Scenario: Keyless operation without any key

- **WHEN** `web_search` runs with no API key from either the override or configuration
- **THEN** the search succeeds against the provider's keyless tier and no Authorization credential is sent

#### Scenario: Provider throttling is transient

- **WHEN** the upstream search provider responds HTTP 429 or times out
- **THEN** the failure is classified transient and dispatched on the backoff-retry path

#### Scenario: Plan-limit rejection is not retried

- **WHEN** the provider rejects the request because the account or plan limit is exhausted
- **THEN** the failure is classified non-retryable and the step fails fast

#### Scenario: Oversized query is a correctable error

- **WHEN** the model calls `web_search` with a query longer than 400 characters
- **THEN** the tool rejects it as a correctable argument error and the model is re-prompted with the constraint
