---
name: copilot-review
description: Read, verify, and answer a GitHub Copilot code review on a Nephtys pull request. Use when a Copilot review lands on a PR, when asked to address review comments, or before merging a PR that has one. Covers the half of the review that the comments API does not return.
---

# Handling a Copilot review

Copilot reviews Nephtys PRs on request. Its findings are usually pointing at
something real, and its *specifics* are wrong often enough that acting on one
without checking the source will put a wrong statement into the repository.

Every command below was run against this repository. Where behaviour was
surprising, what was actually observed is stated.

## 1. Read both halves of the review

A Copilot review has two parts, and the obvious API returns only one.

**Inline findings** — one comment per location:

```bash
gh api repos/AndreaBozzo/Nephtys/pulls/<N>/comments \
  --jq '.[] | "--- \(.path):\(.line // .original_line)  [id=\(.id)]\n\(.body)\n"'
```

**The review body** — the verdict, the file summaries, the counters, and any
**suppressed comments**:

```bash
gh pr view <N> --json reviews \
  --jq '.reviews[] | select(.author.login=="copilot-pull-request-reviewer") | .body'
```

> A suppressed comment is a finding Copilot decided not to post inline. It
> appears **only** in the review body, under `### Suppressed comments (N)`, and
> `pulls/<N>/comments` does not return it. On PR #72 the suppressed comment was
> the sharpest finding in the review: `/readyz` sampled `IsConnected()` and
> `ConnState()` separately, so a response could pair `"status":"ok"` with
> `"state":"RECONNECTING"`. Reading only the inline comments would have missed
> it and shipped it.

The body also carries `Files reviewed`, `Comments generated`, and
`Review effort level` — observed as `Balanced` on a 13-file PR and `Lite` on an
8-file one. A `Lite` review on a large diff is a reason to look harder yourself,
not a clean bill of health.

Identities differ by context: the review author login is
`copilot-pull-request-reviewer`, while the issue timeline names it `Copilot` —
in both the `review_requested` and the `reviewed` event.

## 2. Verify every finding against the source before acting

Treat a finding as a question, not an instruction. Two of the eight findings
received so far were right that something was wrong and wrong about what:

- Copilot said the documented NATS connection states omitted `CONNECTING`,
  `DRAINING (SUBS)` and `DRAINING (PUBS)`. The list *was* wrong, but the client
  returns `DRAINING_SUBS` and `DRAINING_PUBS` — underscores, no parentheses —
  plus a lowercase `unknown status` fallback. Writing the suggested strings into
  the README would have replaced one wrong list with another.

Check the actual source rather than the finding's paraphrase of it:

```bash
grep -n "func (s Status) String" -A 20 "$(go env GOMODCACHE)/github.com/nats-io/nats.go@<version>/nats.go"
```

For findings about Nephtys' own behaviour, the contracts live in
`docs/LIFECYCLE.md` (admission, supervision, failure states) and in the tests.
For findings about the runtime environment — Docker restart semantics, shell
portability, `curl` flags — reproduce the claim locally before agreeing with it.

When a finding is right, fix the cause rather than the line it was reported on.
The JetStream finding on PR #72 was reported at `health.go:51`; the fix was a
new bounded probe in `internal/broker`, a second entry in the readiness
response, and three tests.

## 3. Prove the fix

A fix answering a review still owes the repository what any other change owes
it: a test that fails without it, `make all`, `make check-examples`, and a
`CHANGELOG.md` entry when the change is user-visible. Mutation-check the new
test — revert the fix, watch the test fail, restore it. See
[`../../../docs/CONTRIBUTING.md`](../../../docs/CONTRIBUTING.md).

## 4. Answer each thread, then resolve it

Reply on the thread, saying what changed and what now covers it. Disagreement is
a legitimate reply; say what you checked.

```bash
gh api --method POST \
  repos/AndreaBozzo/Nephtys/pulls/<N>/comments/<COMMENT_ID>/replies \
  -f body='Fixed in <sha>. <what changed, and what test covers it>'
```

Then resolve the threads you have addressed. Thread ids are GraphQL node ids,
not comment ids; a thread's first comment carries `databaseId`, which is the
REST comment id, so findings map to threads through it:

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
  thread { id isResolved } } }' \
  --jq '.data.resolveReviewThread.thread'
```

Resolve only what you actually addressed. An unresolved thread is the record
that something is still open.

## 5. Requesting a review is the maintainer's call

**Do not request a Copilot review unless asked to.** When asked:

```bash
gh pr edit <N> --add-reviewer copilot-pull-request-reviewer
```

The `[bot]` suffix fails — `gh pr edit <N> --add-reviewer 'copilot-pull-request-reviewer[bot]'`
returns `Could not resolve user with login`. Confirm the request landed from the
timeline, not from `requested_reviewers`, which was observed to be empty
immediately after a successful request:

```bash
gh api "repos/AndreaBozzo/Nephtys/issues/<N>/timeline?per_page=100" \
  --jq '[.[] | select(.event=="review_requested" or .event=="reviewed") |
         {event, at: (.created_at // .submitted_at), who: (.requested_reviewer.login // .user.login)}] | .[-5:]'
```

The review took about five minutes to arrive.

## Stacked PRs

A PR based on another feature branch is reviewed against **its own base**, so a
stacked PR's review covers only its own commits. Nephtys' CI runs on every pull
request regardless of base branch, so a stacked PR does get checks.

## Calibration so far

Eight findings across two PRs. Every one pointed at something real; two carried
inaccurate specifics; none was a pure false positive.

| PR | Finding | Verdict |
|---|---|---|
| #72 | `/readyz` checked the connection but not JetStream availability | correct, fixed |
| #72 | Readiness decision and reported state sampled separately *(suppressed)* | correct, fixed |
| #72 | Documented NATS state vocabulary was wrong (×2) | correct finding, wrong replacement strings |
| #72 | Docker does not restart a container for failing its healthcheck (×2) | correct, fixed |
| #73 | `curl -f` discards the body, so the failure message showed nothing | correct, fixed |
| #73 | `set -e` made a failed read inside a poll loop exit silently; `sleep 0.1` is not POSIX | correct, fixed |

What it has been good at: environment semantics, shell portability, and gaps
between what a comment claims and what the code does. What it has been weak at:
the exact contents of an external library's API. Verify those every time.

## Not adopted

Copilot's review body advertises a `.github/skills/code-review/SKILL.md` that
shapes how it reviews the repository. We have not tried it; treat it as an
untested option, not a recommendation.
