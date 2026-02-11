# FAZBD Weekly Report Template (LLM-Oriented)

Purpose: define stable report structure and section semantics so classification is deterministic.

## Output Skeleton

### FortiSoC Cloud

#### Observability stack design
- {{items}}

#### FAZ -> FSM data integration layer (log forwarding)
- {{items}}

### FAZ-BD

#### Release and Support
- **FAZ-BD release**
  - {{items}}
- **Support Cases**
  - {{items}}
- **4500G HW**
  - {{items}}

#### Top Focus
- **FAZ scaling & re-architect**
  - {{items}}
- **External storage support**
  - {{items}}
- **HA Log Sync Enhancement**
  - {{items}}
- **4500G HA AP/AA Support for NHS Case**
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

## Section mapping reference

The renderer will provide a `section_id` for each section in the prompt.
In all classification responses, copy that `section_id` value exactly; do not invent new IDs or reuse the mnemonic labels below unless they are explicitly given as `section_id`.

- FSC_OBS = FortiSoC Cloud > Observability stack design
- FSC_FSM_LOG = FortiSoC Cloud > FAZ -> FSM data integration layer (log forwarding)
- FAZ_REL_RELEASE = FAZ-BD > Release and Support > FAZ-BD release
- FAZ_REL_SUPPORT = FAZ-BD > Release and Support > Support Cases
- FAZ_REL_4500_HW = FAZ-BD > Release and Support > 4500G HW
- FAZ_TOP_SCALE = FAZ-BD > Top Focus > FAZ scaling & re-architect
- FAZ_TOP_EXT_STORAGE = FAZ-BD > Top Focus > External storage support
- FAZ_TOP_HA_LOG_SYNC = FAZ-BD > Top Focus > HA Log Sync Enhancement
- FAZ_TOP_4500_APAA = FAZ-BD > Top Focus > 4500G HA AP/AA Support for NHS Case
- FAZ_INFRA = FAZ-BD > Infrastructure
- FAZ_DATA_PIPELINE = FAZ-BD > Data Pipeline
- FAZ_DATA_AUTOMATION_DB = FAZ-BD > Data Automation & Database
- FAZ_CLUSTER_MGR = FAZ-BD > Cluster Manager
- FAZ_QUERY = FAZ-BD > Query Service
- UND = Undetermined

## Classification Rules (Priority Order)

1. Glossary override wins.
2. If text is customer incident, support session, upgrade issue, crash investigation, workaround, or case follow-up -> FAZ_REL_SUPPORT.
3. If text is ClickHouse, Facet/Facets, query service, logview, fortiview, SQL/hive query behavior -> FAZ_QUERY.
4. If text is backup/restore job framework, storage pool DB job behavior, or schema/data-balance warnings -> FAZ_DATA_AUTOMATION_DB.
5. If text is kafka/ETL/log pipeline lag/streaming/SeaTunnel -> FAZ_DATA_PIPELINE.
6. If text is ansible/playbook/job var map/adom pending/cluster web orchestration -> FAZ_CLUSTER_MGR.
7. If text is routing/subnet/chrony/PTP/firstboot/system infra readiness -> FAZ_INFRA.
8. If text is clearly FortiSoC cloud observability/heartbeat metrics/CKS stack -> FSC_OBS.
9. If text is FAZ->FSM log forwarding/perf forwarding rate -> FSC_FSM_LOG.
10. Otherwise -> UND.

## Tie-Breakers

- Prefer FAZ-BD sections over FortiSoC Cloud when both seem plausible.
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
