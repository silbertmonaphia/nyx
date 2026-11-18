# Spec Delta

## Purpose

A per-user-BYOK cross-encoder rerank stage that reorders over-fetched retrieval candidates before they are returned or fused, raising first-result fidelity without changing the index, the embedding identity, or the caller's visible corpus.

## ADDED Requirements

### Requirement: Reranking is a per-user second stage

A retrieval SHALL apply the rerank stage when the caller's stored configuration contains a rerank group. When enabled, the retrieval SHALL first over-fetch a candidate pool from the existing vector search and then reorder that pool with a cross-encoder before returning the requested number of passages. The pool SHALL contain at least the configured candidate count and at least the requested result count, whichever is larger. When the caller has no rerank group, retrieval output SHALL be identical to the dense-only result for the same input.

#### Scenario: Configured user's retrieval reranks an over-fetched pool

- **WHEN** the caller has a rerank group configured and 5 results are requested with a configured candidate count of 20
- **THEN** at least 20 candidates are searched and the 5 returned passages follow reranker order

#### Scenario: Unconfigured user's retrieval is unchanged

- **WHEN** the caller has no rerank group
- **THEN** the returned passages and their order are exactly the dense-only result and no reranker endpoint is contacted

#### Scenario: Requested count above the candidate count

- **WHEN** a caller requests more results than the configured candidate count
- **THEN** the candidate pool is at least the requested count and the requested count is returned

### Requirement: Every corpus reranks before fusion

A `feeds`, `kb`, or `both` retrieval SHALL apply the rerank stage to each named corpus's candidate list. A `both` retrieval SHALL rerank each corpus independently and then fuse the two post-rerank lists by rank, so a reranker score never competes directly with a score from the other corpus.

#### Scenario: KB-only retrieval reranks its candidates

- **WHEN** a retrieval names the `kb` corpus and the caller has a rerank group
- **THEN** the knowledge candidates are reordered by the reranker before the requested results are returned

#### Scenario: Feeds-only retrieval reranks its candidates

- **WHEN** a retrieval names the `feeds` corpus and the caller has a rerank group
- **THEN** the feed candidates are reordered by the reranker before the requested results are returned

#### Scenario: Both-corpus fusion consumes post-rerank ranks

- **WHEN** a retrieval names the `both` corpus and the reranker reorders one corpus
- **THEN** the fused order follows each corpus's post-rerank rank and no raw reranker score is compared across corpora

### Requirement: Rerank configuration is resolved per caller

The rerank endpoint, API key, and model SHALL be resolved from the calling user's own stored configuration on every retrieval. No deployment-level reranker SHALL serve as a fallback for a user without a rerank group. The API key SHALL be decrypted only for the outbound request and MUST NOT appear in logs, trace spans, metrics, error details, or the agent payload. One user's rerank configuration MUST NOT affect another user's retrieval.

#### Scenario: Traffic reaches the caller's own endpoint

- **WHEN** two configured users have different rerank endpoints and each retrieves
- **THEN** each request reaches that user's own endpoint with that user's model

#### Scenario: No deployment fallback

- **WHEN** a user has no rerank group while other users do
- **THEN** that user's retrieval makes no reranker call and returns dense order

#### Scenario: Key material stays out of telemetry

- **WHEN** a rerank request is made with a configured API key
- **THEN** no log line, span attribute, metric label, or response body contains the key

### Requirement: Reranker failure degrades to dense order

A reranker timeout, connection failure, non-2xx response, or malformed response SHALL NOT fail the retrieval. The stage SHALL log the failure, return the dense-order result for that corpus, and record the outcome in telemetry. After repeated consecutive failures for one caller's configuration the system SHALL stop calling the reranker for a cooldown period, and a post-cooldown probe that succeeds SHALL restore reranking.

#### Scenario: Timeout falls back to dense order

- **WHEN** the reranker does not answer within the configured timeout
- **THEN** the retrieval returns the dense-ordered passages after at most the configured timeout and chat still completes

#### Scenario: Upstream error falls back to dense order

- **WHEN** the reranker returns a 5xx or a body that cannot be parsed
- **THEN** the retrieval returns the dense-ordered passages and logs the failure without surfacing the upstream body to the caller

#### Scenario: Repeated failures open a cooldown

- **WHEN** the reranker has failed consecutively past the failure threshold and the cooldown has not elapsed
- **THEN** no reranker call is made and dense order is returned

#### Scenario: Recovery restores reranking

- **WHEN** a probe after the cooldown succeeds
- **THEN** subsequent retrievals use the reranker again

### Requirement: Reranker output is validated before it reorders passages

The stage SHALL treat the reranker response as untrusted: it SHALL accept at most one score per candidate index, ignore out-of-range or duplicate indices and any echoed document text, and score each candidate at most once. Candidates absent from the response SHALL keep their dense order after the scored candidates. Equal scores SHALL break by dense rank ascending, so the result is deterministic.

#### Scenario: Out-of-range and duplicate indices are ignored

- **WHEN** the response names an index outside the candidate list or repeats an index
- **THEN** those entries have no effect and every candidate is scored at most once

#### Scenario: Unscored candidates keep dense order

- **WHEN** the response scores only part of the candidate pool
- **THEN** the unscored candidates follow the scored ones in their original dense order

#### Scenario: Equal scores break deterministically

- **WHEN** two candidates receive the same score
- **THEN** the candidate that ranked earlier in the dense result comes first

#### Scenario: The reranker cannot add passages

- **WHEN** the reranker response refers to more passages than the candidate pool contained
- **THEN** no passage outside the scoped candidate pool appears in the result

### Requirement: Rerank inputs are bounded and stay within the caller's scope

The stage SHALL send only passages already selected by the scoped vector search, so it cannot introduce a passage from outside the caller's visible corpus. Each passage's rerank text SHALL be truncated to a configured UTF-8-safe byte cap, and the query plus the capped texts SHALL be the only content sent to the reranker.

#### Scenario: Only scoped candidates are sent

- **WHEN** a user's retrieval is scoped to their own sources
- **THEN** the reranker receives only passages already returned by that scoped search

#### Scenario: Oversized passages are capped

- **WHEN** a candidate's rerank text exceeds the configured cap
- **THEN** the text sent to the reranker is truncated to the cap before the request is made

### Requirement: Rerank operational bounds are validated at startup

The deployment SHALL provide the candidate count, request timeout, and per-document byte cap as configuration with defaults of 20 candidates, 2 seconds, and 4000 bytes. Each SHALL be positive and bounded; an invalid value SHALL fail startup with an error naming the variable. These bounds SHALL apply uniformly to every caller and MUST NOT be settable per user.

#### Scenario: Invalid timeout is refused

- **WHEN** the service starts with the rerank timeout at zero or negative
- **THEN** startup fails with an error naming the variable

#### Scenario: Invalid candidate count is refused

- **WHEN** the candidate count is zero, negative, or above the documented maximum
- **THEN** startup fails with an error naming the variable

#### Scenario: Bounds are deployment-wide

- **WHEN** a user configures a rerank endpoint
- **THEN** the candidate count, timeout, and byte cap remain the deployment's values

### Requirement: Rerank outcomes are observable

Each rerank attempt SHALL produce a trace span carrying the model, candidate count, returned count, and whether the dense fallback was used, plus a metric counter distinguishing success, fallback, and cooldown skips. The span and metric MUST NOT carry passage text or the API key.

#### Scenario: Span records the outcome

- **WHEN** a rerank call succeeds
- **THEN** its span carries the model, candidate count, and returned count with no passage text

#### Scenario: Fallback is counted

- **WHEN** a rerank call fails and dense order is returned
- **THEN** a fallback outcome is counted and the span records the failure
