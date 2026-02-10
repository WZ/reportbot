# Weekly Report Classification Guide (LLM-Oriented)

Purpose: define stable report structure and section semantics so classification is deterministic.

## Output Skeleton

### Product Alpha

#### Observability stack design
- {{items}}

#### Cross-system data integration layer (log forwarding)
- {{items}}

### Product Beta

#### Release and Support
- **Product Beta release**
  - {{items}}
- **Support Cases**
  - {{items}}
- **Hardware Platform**
  - {{items}}

#### Top Focus
- **Core platform scaling and re-architecture**
  - {{items}}
- **External storage support**
  - {{items}}
- **HA log sync enhancement**
  - {{items}}
- **Hardware HA Support for Priority Customer**
  - {{items}}

#### Infrastructure
- {{items}}

#### Data Pipeline
- {{items}}

#### Data Automation & Database
- {{items}}

#### Cluster Manager
- {{items}}

#### Query Service
- {{items}}

#### Undetermined
- {{items}}

## Section IDs (Stable)

Use these IDs in LLM classification responses; renderer maps IDs to headings above.

- PA_OBS = Product Alpha > Observability stack design
- PA_LOG = Product Alpha > Cross-system data integration layer (log forwarding)
- PB_REL_RELEASE = Product Beta > Release and Support > Product Beta release
- PB_REL_SUPPORT = Product Beta > Release and Support > Support Cases
- PB_REL_HW = Product Beta > Release and Support > Hardware Platform
- PB_TOP_SCALE = Product Beta > Top Focus > Core platform scaling and re-architecture
- PB_TOP_EXT_STORAGE = Product Beta > Top Focus > External storage support
- PB_TOP_HA_LOG_SYNC = Product Beta > Top Focus > HA log sync enhancement
- PB_TOP_HW_HA = Product Beta > Top Focus > Hardware HA Support for Priority Customer
- PB_INFRA = Product Beta > Infrastructure
- PB_DATA_PIPELINE = Product Beta > Data Pipeline
- PB_DATA_AUTOMATION_DB = Product Beta > Data Automation & Database
- PB_CLUSTER_MGR = Product Beta > Cluster Manager
- PB_QUERY = Product Beta > Query Service
- UND = Undetermined

## Classification Rules (Priority Order)

1. Glossary override wins.
2. If text is customer incident, support session, upgrade issue, crash investigation, workaround, or case follow-up -> PB_REL_SUPPORT.
3. If text is TimescaleDB, analytics dashboard, query service, log viewer, event viewer, SQL/hive query behavior -> PB_QUERY.
4. If text is backup/restore job framework, storage pool DB job behavior, or schema/data-balance warnings -> PB_DATA_AUTOMATION_DB.
5. If text is kafka/ETL/log pipeline lag/streaming/data pipeline framework -> PB_DATA_PIPELINE.
6. If text is ansible/playbook/job var map/tenant pending/cluster web orchestration -> PB_CLUSTER_MGR.
7. If text is routing/subnet/NTP/firstboot/system infra readiness -> PB_INFRA.
8. If text is clearly Product Alpha observability/heartbeat metrics/CKS stack -> PA_OBS.
9. If text is cross-system log forwarding/perf forwarding rate -> PA_LOG.
10. Otherwise -> UND.

## Tie-Breakers

- Prefer Product Beta sections over Product Alpha when both seem plausible.
- Prefer PB_QUERY over PB_REL_SUPPORT if query/TimescaleDB/analytics dashboard terms appear.
- For duplicate/near-duplicate item texts in the same run, keep one (highest confidence) and preserve latest status.

## Status Normalization

Allowed output statuses:
- done
- in testing
- in progress
- other

Normalize synonyms:
- in qa / in test -> in testing
- wip / ongoing -> in progress

## Item Formatting Rules

- Keep one ticket prefix only. If description already starts with [12345], do not add another.
- Team mode format: `- **Author** - Description (status)`
- Boss mode format: `- Description (status)`
- Keep original heading order exactly as template.

## LLM Response Contract (recommended)

For each item return JSON object:
- id
- section_id (one of IDs above)
- normalized_status
- ticket_ids
- duplicate_of (optional existing key)
- confidence (0..1)
