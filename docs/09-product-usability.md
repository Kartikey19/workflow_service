# Workflow setup: Railway research and Weft decisions

Reviewed 12 October 2026. This is an examination of public documentation and UI examples, not Railway’s internal user-research process.

## Observed patterns

Railway uses its [project canvas](https://docs.railway.com/projects) as the home for related resources. Tiles expose identity and status, and selecting a [service](https://docs.railway.com/services) opens its configuration. Creation starts with New or the command palette. The useful principle for Weft is to keep the workflow visible while its dependencies are configured.

[Staged changes](https://docs.railway.com/deployments/staged-changes) have a visible count, a review of previous and proposed values, and an explicit apply action. Weft already publishes immutable versions: keep editing separate from publishing and put validation beside the affected step. Credential setup saves a reusable account immediately; publishing applies only the workflow definition. These are different actions and need distinct copy.

[Bring Your Own Models](https://docs.railway.com/ai/bring-your-own-models) separates account connection from model choice. Its model list groups accounts, a default applies across agent surfaces, and a local selection overrides that default. OpenRouter sign-in uses a popup and credentials appear masked. This feature is in Priority Boarding. For Weft, preserve reusable agent defaults while allowing each workflow step to independently override the provider account or model.

[Deployment logs](https://docs.railway.com/observability/logs) are accessible from the deployment being diagnosed, with broader search available separately. For Weft, a failed run should lead directly to the failed step’s input, output and error, rather than making users reconstruct context on another page.

## First implementation

- Always offer an AI step, even with no saved agent or provider account. Let users write a prompt and select a model before connecting credentials.
- Add provider and connector accounts in focused modals over the workflow. Returning from OAuth must preserve the draft; external consent uses a separate window.
- Give every account a client-specific name. Multiple keys for the same provider are separate accounts, not replacements.
- Keep account and model defaults on reusable agents. Step overrides affect only that step. An explicit “Use agent default” removes the override.
- Display recognizable locally bundled app icons in the catalog, canvas and account screens.
- Show unavailable OAuth setup with an explanation, rather than hiding the app or redirecting the user.

## Acceptance criteria

Start a workflow without an agent or saved provider, enter its model and prompt, connect two accounts for one provider, select the second account, and publish without navigating away. Verify that secrets never enter the YAML. Set a model-only override on a reusable agent, run it, and verify that its saved account, instructions and model default remain unchanged. Cancel setup and use Escape without losing the draft. Test keyboard focus and narrow screens.

## Recoverable workflow editing

The editor now saves unpublished definitions locally, scoped to the signed-in user and organization. Existing workflows have separate draft slots; each workspace also has one new-workflow draft. Refresh reopens the active saved draft. After returning to the workflow list, Resume draft opens an unfinished new workflow and an Unpublished draft label identifies edited published workflows.

Canvas and YAML edits use one history. Undo/redo restores definition changes, including deleted steps and their configuration; canvas positions remain separate. Typing in one field is grouped into an edit. The latest draft survives refresh, while up to 80 undo entries remain in memory for the current editing session. Text fields retain native keyboard undo; toolbar actions and shortcuts outside text fields use workflow history. Revert changes returns to the published baseline (or the empty new workflow) and can itself be undone.

Account setup remains outside workflow history. Saved account credentials are never copied into the draft store by setup forms. Workflow definitions can contain prompts and sample data; local drafts are browser data, not encrypted server drafts. Publishing clears the submitted draft while preserving subsequent edits. A newer published version or changed browser storage never silently replaces the current definition: review the notice, or copy YAML before reloading when another tab changed the saved drafts. Storage failure is visible and unsaved changes trigger a browser warning when closing or reloading the tab.

Browser coverage exercises refresh during a pending field edit, invalid YAML recovery, shared canvas/YAML undo, deleted-step restoration, publication cleanup, account isolation, newer-version conflicts, setup-key exclusion and storage failures. Unit coverage also checks edits during publication and detection of changes from another tab.

## Later refinements

Investigate a publish review showing changed steps and account references, a searchable command palette, starter workflows, and richer failed-step diagnostics. Prioritize these using observed user completion rates and support reports; a competitor’s interface alone is not evidence that every pattern belongs in Weft.

See [Railway feature opportunities](10-railway-feature-opportunities.md) for the expanded comparison, current Weft foundations, recommended sequence and implementation boundaries.
