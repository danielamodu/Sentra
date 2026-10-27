# Sentra GitLab Integration Boundary Specification

This document defines the exact boundary between local, in-process implementations and remote GitLab cloud/infrastructure operations in Sentra.

## 1. Local Implementations (Executable Offline Without Remote Infrastructure)
- **Branch Naming Conventions**: Formats deterministic Git branches matching regex 'ops/proposal-<id>' via 'gitlab.FormatProposalBranchName'.
- **Artifact Bundling**: Packages 'proposal.json', 'config.diff', 'verification_report.json', and 'evidence_summary.md' via 'gitlab.BundleProposalArtifacts'.
- **Merge Request Markdown Generation**: Generates structured MR description text with candidate metrics, verifier summary, and policy checklist via 'gitlab.GenerateMergeRequestDescription'.
- **CI Adjudication Pipeline Runner**: Executes 10 discrete verification stages in-process via 'gitlab.RunCIPipeline'.
- **Controlled Promotion Controller**: Enforces HMAC-SHA256 signature verification, config drift checks, and post-promotion simulation/verification via 'gitlab.ExecutePromotion'.
- **Declarative CI Spec**: Defines a 5-stage pipeline in '.gitlab-ci.yml'.

## 2. Remote Infrastructure Requirements (External GitLab Infrastructure)
- **Remote MR Creation**: Calling GitLab REST API 'POST /api/v4/projects/:id/merge_requests' requires 'GITLAB_TOKEN' and network access to gitlab.com or self-hosted GitLab.
- **Remote Git Push**: Pushing proposal branches requires remote write access to a GitLab repository URL.
- **Remote CI Runner Execution**: Executing '.gitlab-ci.yml' jobs in Docker/VMs requires a registered GitLab Runner daemon.
- **Interactive UI Approvals**: Human approval via GitLab Web UI requires GitLab Enterprise Edition / Ultimate merge request approval rules.
