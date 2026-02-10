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

- FSC_OBS = Product Alpha > Observability stack design
- FSC_FSM_LOG = Product Alpha > Cross-system data integration layer (log forwarding)
- FAZ_REL_RELEASE = Product Beta > Release and Support > Product Beta release
- FAZ_REL_SUPPORT = Product Beta > Release and Support > Support Cases
- FAZ_REL_4500_HW = Product Beta > Release and Support > Hardware Platform
- FAZ_TOP_SCALE = Product Beta > Top Focus > Core platform scaling and re-architecture
- FAZ_TOP_EXT_STORAGE = Product Beta > Top Focus > External storage support
- FAZ_TOP_HA_LOG_SYNC = Product Beta > Top Focus > HA log sync enhancement
- FAZ_TOP_4500_APAA = Product Beta > Top Focus > Hardware HA Support for Priority Customer
- FAZ_INFRA = Product Beta > Infrastructure
- FAZ_DATA_PIPELINE = Product Beta > Data Pipeline
- FAZ_DATA_AUTOMATION_DB = Product Beta > Data Automation & Database
- FAZ_CLUSTER_MGR = Product Beta > Cluster Manager
- FAZ_QUERY = Product Beta > Query Service
- UND = Undetermined

## Classification Rules (Priority Order)

1. Glossary override wins.
2. If text is customer incident, support session, upgrade issue, crash investigation, workaround, or case follow-up -> FAZ_REL_SUPPORT.
3. If text is ClickHouse, Facet/Facets, query service, logview, fortiview, SQL/hive query behavior -> FAZ_QUERY.
4. If text is backup/restore job framework, storage pool DB job behavior, or schema/data-balance warnings -> FAZ_DATA_AUTOMATION_DB.
5. If text is kafka/ETL/log pipeline lag/streaming/SeaTunnel -> FAZ_DATA_PIPELINE.
6. If text is ansible/playbook/job var map/adom pending/cluster web orchestration -> FAZ_CLUSTER_MGR.
7. If text is routing/subnet/chrony/PTP/firstboot/system infra readiness -> FAZ_INFRA.
8. If text is clearly Product Alpha observability/heartbeat metrics/CKS stack -> FSC_OBS.
9. If text is cross-system log forwarding/perf forwarding rate -> FSC_FSM_LOG.
10. Otherwise -> UND.

## Tie-Breakers

- Prefer Product Beta sections over Product Alpha when both seem plausible.
- Prefer FAZ_QUERY over FAZ_REL_SUPPORT if query/ClickHouse/Facet terms appear.
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
