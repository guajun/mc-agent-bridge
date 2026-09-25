# Development Workflow

When asked to implement an issue or feature, complete the implementation and prepare a pull request for human review.

1. Read this guide, any more specific `AGENTS.md` files, the linked issue, and the relevant code before editing. Check `git status` first and preserve unrelated user changes.
2. Create an isolated Git worktree for the task on a topic branch based on the repository default branch. Do not implement in the shared checkout, and never commit directly to `main`.
3. Implement the requested behavior, update relevant documentation, and add or update focused tests when the change warrants them. Keep the Bridge Harness-neutral; do not add model calls or per-Harness session behavior.
4. Run the relevant tests, build, or documentation checks available in this repository. Review the complete diff for accidental files, secrets, and unrelated changes. Report checks that could not run and why.
5. Commit the finished change, push the topic branch, and open a non-draft pull request against the default branch. Link the issue and include a concise summary, verification results, and any remaining limitations.
6. Stop after opening the pull request and leave it for human review. Do not merge it, close the issue, or force-push unless the user explicitly asks.

If blocked by missing requirements or an external dependency, do the independent work, then report the concrete blocker instead of claiming the task is complete.
