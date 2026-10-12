# Railway feature research: opportunities for Weft

Reviewed 12 October 2026 against public Railway documentation and Weft's current source, including the workflow-setup branch. These are proposals, not implemented features or commitments. Priority reflects the current goal of helping early users and agencies build useful automations; it has not been validated through user interviews. No hosting changes are proposed. Draft recovery and undo/redo have since been implemented on the workflow-setup branch; see [recoverable editing](09-product-usability.md#recoverable-workflow-editing). The other opportunities remain proposals.

## What Railway makes easier

| Railway feature and observed behavior | Proposed Weft adaptation | Priority |
| --- | --- | --- |
| [Keyboard navigation](https://docs.railway.com/overview/keyboard-shortcuts): command palette, canvas undo/redo, contextual Escape | Undo/redo for workflow edits; searchable commands that open setup or inspection over the current workflow | First: editing recovery; later: global palette |
| [Templates](https://docs.railway.com/templates/deploy): configured starting points and a required-variable setup flow | A small starter gallery with sample input, account requirements and an in-builder setup checklist | First |
| [Reference variables](https://docs.railway.com/variables): reuse values across resources with autocomplete | Pick an earlier step's output field visually, with its type, example value and resulting expression | First |
| [Logs](https://docs.railway.com/observability/logs): deployment-specific inspection plus an environment-wide explorer | Test and inspect a workflow in its builder; open a failed step directly, preserving run/version context | First |
| [Staged changes](https://docs.railway.com/deployments/staged-changes): count changes, inspect a diff and explicitly apply | A publish review showing added/removed steps, changed models, accounts and mappings | Next |
| [Deployment recovery](https://docs.railway.com/deployments/deployment-actions): distinct rollback, redeploy and restart actions | Distinguish restoring a workflow definition, starting a fresh retry, and retrying an eligible active-run step | Next |
| [Environments](https://docs.railway.com/environments): persistent isolated configurations and temporary PR environments | Test/Live configurations with explicit account bindings and controlled promotion | Later: significant backend work |
| [Project members](https://docs.railway.com/projects/project-members): scoped Owner, Editor and Viewer permissions | Make existing organization boundaries clear as client workspaces; simplify client invitations and switching | Next when onboarding multiple clients |
| [Cost control](https://docs.railway.com/pricing/cost-control): soft alerts and independent hard limits for compute and agent usage | Aggregate workflow/account/model usage; then add configurable AI budget alerts and execution limits | Next: reporting; later: enforcement |
| [Observability](https://docs.railway.com/observability): grouped dashboards and threshold monitors | A fixed workflow health view and actionable failure/credential alerts before customizable dashboards | Next |
| [Cron jobs](https://docs.railway.com/cron-jobs): schedule configuration with explicit behavior when a previous job remains active | In-builder Manual/Webhook/Schedule trigger setup, next-run preview and overlap policy | Webhook UI next; schedules later |
| [Contextual agent](https://docs.railway.com/cli/agent): environment/service-scoped questions and continued conversations | Explain a selected step or failed run; propose a validated workflow patch for review | Later, after inspection and publish review |

## First release: help someone finish an automation

### 1. Recoverable editing

The current builder persists node positions locally; the workflow definition is React state. Add draft autosave, a visible saved/unsaved state, restore after refresh, and undo/redo for definition edits. This is a Weft proposal; Railway's shortcut documentation establishes undo/redo, not Weft-style draft autosave.

Scope local drafts to organization, user and workflow. Keep account secrets in setup forms, outside history. Treat prompts and sample inputs as potentially sensitive; make local recovery clear and removable. Later server drafts need permissions and optimistic revisions to prevent one editor overwriting another. Undoing a workflow edit must not imply deleting an account that was already saved.

Acceptance: edit a model, add/remove a step, undo/redo, reload and recover the unpublished definition without changing the published version.

### 2. Curated starter workflows

Start with three validated examples: classify a webhook support ticket and notify Slack; summarize supplied text and send a Slack digest; request human approval before an outbound action. Add Gmail-based examples once the operator configures Google OAuth.

Each template includes a short outcome, sample input, expected output and required account slots. Instantiation opens an editable draft and collects account choices in place. Importing or opening a template does not execute it. Reuse the executor catalog, schemas, compiler and the existing support-ticket example. Validate templates against the current compiler as part of their build workflow.

Acceptance: choose a starter, connect its accounts, preview its mappings and complete a sample run without writing YAML.

### 3. Visual output mapping

Offer “Use data from another step” beside supported input fields. Show upstream outputs as a field tree with types and examples, then insert the existing expression syntax. Begin with schema-backed values and previous test outputs; label observed samples separately from declared types. Provide an advanced expression editor as needed.

Only offer dependency-valid sources. A conditional step might not execute and a sample may omit a field, so expose missing/null handling. Renaming a source step must preserve valid references or show a targeted diagnostic.

Acceptance: map a classifier's result to a Slack message, see the generated expression, and get a clear error when a required source value is unavailable.

### 4. Test and inspect without leaving the canvas

Weft already validates definitions without execution, runs immutable published versions, streams events and shows per-attempt inputs, outputs, errors and AI metrics. Add a run drawer with sample inputs, status on canvas nodes and a selected-step inspector. Initially use the existing published-version execution contract; clearly identify which version ran.

Testing an unpublished draft or a single arbitrary step needs an explicit execution contract and dependency inputs. Do not pretend compiler validation is a run or simulate a successful connector call. Distinguish validation, supplied fixtures and real execution. Real Slack/email/database actions have side effects; show the selected account and action before executing. A test account can still write real data.

Acceptance: run a supported test, inspect the failed node directly, correct the draft, and understand whether the displayed result belongs to the edited definition or the published version.

## Subsequent work and boundaries

Publish review should compare normalized workflow definitions, showing account references but never secret values. Immutable workflow versions already exist. Restore should create a new version from an older definition, preserving history. It must explain which webhook/version bindings change. Reusable agents are currently resolved at execution time; reproducing old behavior also needs agent revision snapshots or explicit version pinning. Restoring a definition cannot undo an email already sent.

Client workspaces should build on the existing organizations, memberships, invitations and organization switcher. Multiple named accounts inside one organization help identify clients, but do not isolate access between clients. Use separate organizations for clients requiring isolation, and explicitly remap credentials when copying workflow definitions across them.

Usage reporting can aggregate existing node token, duration and estimated-cost metrics by workflow, model and account. Preserve “unknown cost” for unpriced models. A hard AI budget requires atomic reservations across concurrent runs and reconciliation after calls; checking a dashboard total before execution cannot guarantee a cap or equal the provider's invoice. Provider billing remains external.

A health view should show success/failure counts, duration, waiting runs and recent errors, with links to the relevant attempts. Use a fixed useful view first. Alert on terminal failures, expired credentials or missed schedules with deduplication and recovery messages; avoid notifying for every retry. Railway's monitors concern infrastructure thresholds, while these would be Weft application events.

Webhook setup exists in settings today and can move into a workflow trigger panel. Recurring schedule storage and dispatch are new work: define timezone, daylight-saving behavior, next-run preview, missed-run policy and skip/queue/allow overlap. Reuse durable PostgreSQL execution; do not add a separate always-on scheduler container merely for the UI. The current production worker's ten-minute polling interval must be reflected in timing expectations until dispatch is improved.

A contextual assistant can build on Weft's existing validated draft generator. Supply the selected workflow and redacted diagnostics, ask it to explain a failure or propose an edit, and show a diff before applying. Report evidence versus hypotheses. Publishing, rerunning and external actions remain separate explicit operations. Read-only explanations should come before autonomous repairs or a public workflow-management MCP API.

## How to validate the priorities

Observe new users building the same starter workflow and record where they hesitate or leave the builder. Measure time to first successful run, completion rate, recovered drafts, validation failures and how long it takes to diagnose a failed step. Collect why users abandon setup, including OAuth configuration gaps. Keep telemetry to event names, counts and durations; exclude credentials, prompts and workflow payloads. Add the smallest metrics needed to test a specific improvement rather than a broad analytics system.

Recommended sequence: recoverable editing, starter workflows, data mapping and contextual testing; then publish review, client workspace onboarding and health/usage reporting. Test/Live environments, recurring schedules and contextual AI diagnosis follow demonstrated demand. These priorities target user completion; copying a competitor's feature list alone does not establish value.
