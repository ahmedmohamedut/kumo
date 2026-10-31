# Architecture Decisions — kumo

## ADR-001 — Project initialised

Date: 2026-10-31
Status: Accepted

Decision: Project initialised via the Gestalt platform.
Description: Kumo is an open-source, AWS-style private cloud for teams that find OpenStack too heavy and Proxmox too low-level. One Go binary, one web console, one SQLite file. No message bus cluster, no dozen microservices.
Stack: go / gin / sqlite / react (web console)
Architecture: Layered monolith (api -> service -> store/hypervisor); harness adapted from the corporate-ops-web-mobile template, tier 1
