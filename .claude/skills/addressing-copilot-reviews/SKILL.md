---
name: addressing-copilot-reviews
description: Reads, verifies, and answers a GitHub Copilot code review on a Nephtys pull request, including the suppressed findings the pull-request comments API does not return. Covers reading both halves of a review, checking each finding against the source before acting, replying to and resolving review threads with gh, and requesting a re-review. Use when a Copilot review lands on a PR, when asked to address review comments or review feedback, or before merging a PR that has an open review.
---

# Addressing a Copilot review

Copilot reviews Nephtys PRs on request. Its findings usually point at something
real; its *specifics* are wrong often enough that acting on one without checking
the source will put a wrong statement into the repository.

Terms used below: a **finding** is the substance; a **comment** is the REST
object carrying it (`.id`); a **thread** is the GraphQL object holding a comment
and its replies (`PRRT_...`).

```
- [ ] 1. Read both halves of the review
- [ ] 2. Verify each finding against the source
- [ ] 3. Fix the cause, with a test that fails without the fix
- [ ] 4. Reply on each thread, then resolve the ones addressed
- [ ] 5. Append what happened to the calibration log
```

## 1. Read both halves of the review

Inline findings:

```bash
gh api repos/AndreaBozzo/Nephtys/pulls/<N>/comments \
  --jq '.[] | "--- \(.path):\(.line // .original_line)  [id=\(.id)]\n\(.body)\n"'
```

The review body, which carries the verdict, file summaries, counters, and
suppressed findings:

```bash
gh pr view <N> --json reviews \
  --jq '.reviews[] | select(.author.login=="copilot-pull-request-reviewer") | .body'
```

Read both. A suppressed finding is one Copilot chose not to post inline: it
appears only in the review body under `### Suppressed comments (N)`, and
`pulls/<N>/comments` does not return it. On PR #72 the suppressed finding was
the sharpest one in the review — `/readyz` sampled `IsConnected()` and
`ConnState()` separately, so a response could pair `"status":"ok"` with
`"state":"RECONNECTING"`. Reading only the inline comments would have shipped it.

The body also reports `Files reviewed`, `Comments generated`, and
`Review effort level`. A `Lite` effort level on a large diff is a reason to look
harder yourself, not a clean bill of health.

The review author login is `copilot-pull-request-reviewer`; the issue timeline
names it `Copilot`.

## 2. Verify each finding against the source

Treat a finding as a question. Check the claim, not its paraphrase:

```bash
grep -n "func (s Status) String" -A 20 \
  "$(go env GOMODCACHE)/github.com/nats-io/nats.go@<version>/nats.go"
```

Nephtys' own contracts live in `docs/LIFECYCLE.md` (admission, supervision,
failure states) and in the tests. Claims about the runtime environment — Docker
semantics, shell portability, curl flags — get reproduced locally before being
agreed with.

Fix the cause, not the line the finding was reported on. The JetStream finding
on PR #72 was reported at `health.go:51`; the fix was a bounded probe in
`internal/broker`, a second entry in the readiness response, and three tests.

## 3. Prove the fix

Same bar as any other change here: a test watched to fail without the fix,
`make all`, `make check-examples`, and a `CHANGELOG.md` entry when the change is
user-visible. See [CONTRIBUTING.md](../../../docs/CONTRIBUTING.md).

## 4. Reply, then resolve

Say what changed and what covers it. Disagreement is a legitimate reply; say
what was checked.

```bash
gh api --method POST \
  repos/AndreaBozzo/Nephtys/pulls/<N>/comments/<COMMENT_ID>/replies \
  -f body='Fixed in <sha>. <what changed, and what test covers it>'
```

Thread ids are GraphQL node ids, not comment ids. A thread's first comment
carries `databaseId`, which is the REST comment id — that is the mapping from a
finding to its thread:

```bash
gh api graphql -f query='
query {
  repository(owner: "AndreaBozzo", name: "Nephtys") {
    pullRequest(number: <N>) {
      reviewThreads(first: 20) {
        nodes { id isResolved path comments(first: 1) { nodes { databaseId } } }
      }
    }
  }
}' --jq '.data.repository.pullRequest.reviewThreads.nodes[] |
         "\(.id) resolved=\(.isResolved) \(.path) comment=\(.comments.nodes[0].databaseId)"'

gh api graphql -f query='
mutation { resolveReviewThread(input: {threadId: "<THREAD_ID>"}) {
  thread { id isResolved } } }' --jq '.data.resolveReviewThread.thread'
```

Resolve only what was addressed. An unresolved thread is the record that
something is still open.

## 5. Requesting a review is the maintainer's call

Do not request a Copilot review unless asked to. When asked:

```bash
gh pr edit <N> --add-reviewer copilot-pull-request-reviewer
```

The `[bot]` suffix fails: `--add-reviewer 'copilot-pull-request-reviewer[bot]'`
returns `Could not resolve user with login`. Confirm from the timeline, not from
`requested_reviewers`, which reads empty immediately after a successful request:

```bash
gh api "repos/AndreaBozzo/Nephtys/issues/<N>/timeline?per_page=100" \
  --jq '[.[] | select(.event=="review_requested" or .event=="reviewed") |
         {event, at: (.created_at // .submitted_at), who: (.requested_reviewer.login // .user.login)}] | .[-5:]'
```

A review took about five minutes to arrive.

## Stacked PRs

A PR based on another feature branch is reviewed against its own base, so the
review covers only that PR's commits. Nephtys' CI runs on every pull request
regardless of base branch, so a stacked PR does get checks. Merging one takes
`PUT /repos/{owner}/{repo}/pulls/<N>/merge-async`; `gh pr merge` and the ordinary
merge endpoint both refuse it.

## Calibration log

Append after each review. It records where these reviews have been reliable, so
the next reader does not have to re-learn it.

| PR | Finding | Verdict |
|---|---|---|
| #72 | `/readyz` checked the connection but not JetStream availability | correct, fixed |
| #72 | Readiness decision and reported state sampled separately (suppressed) | correct, fixed |
| #72 | Documented NATS state vocabulary was wrong (x2) | correct finding, wrong replacement strings |
| #72 | Docker does not restart a container for failing its healthcheck (x2) | correct, fixed |
| #73 | `curl -f` discards the body, so the failure message showed nothing | correct, fixed |
| #73 | `set -e` made a failed read in a poll loop exit silently; `sleep 0.1` is not POSIX | correct, fixed |

Eight findings over two PRs: all pointed at something real, two carried
inaccurate specifics, none was a pure false positive. Reliable on environment
semantics, shell portability, and gaps between what a comment claims and what
the code does. Weak on the exact contents of an external library's API — verify
those every time.

## Not adopted

Copilot's review body advertises a `.github/skills/code-review/SKILL.md` that
shapes how it reviews a repository. Untried here: an option, not a
recommendation.
