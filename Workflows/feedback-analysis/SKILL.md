---
name: feedback-analysis
description: Analyze open apartment-hunter listing feedbacks stored in Postgres, implement code fixes, test them, commit the changes with feedback IDs in the commit message, and mark feedbacks as resolved in the database.
---

# Feedback Analysis

Use this skill when asked to process apartment-hunter feedbacks, fix issues reported through the Telegram bot, or resolve `listing_feedbacks` rows.

## Workflow

1. Inspect repo state with `git status --short`. Do not revert unrelated user changes.
2. Read open feedbacks from Postgres:

```sql
select id, listing_id, profile_id, feedback_text, created_at
from listing_feedbacks
where status = 'open'
order by created_at asc, id asc;
```

3. For each feedback, load the related evaluation context:

```sql
select listing_id, source, listing, score, insights, report, status, failure_reason
from listing_evaluations
where id = $1;
```

4. Group feedbacks by likely root cause. Prefer fixing one coherent root cause per commit.
5. Implement the smallest code change that addresses the feedback. Add or update focused tests around the regression.
6. Run the relevant test package first, then `go test ./...` when practical.
7. Commit only the files needed for the fix. Include feedback IDs in the commit message, for example:

```text
Fix location parsing for map links

Resolves feedback: #12, #14
```

8. After the commit succeeds, mark only the feedbacks covered by that commit as resolved:

```sql
update listing_feedbacks
set status = 'resolved',
    resolved_commit = $1,
    resolution_note = $2,
    resolved_at = now(),
    updated_at = now()
where id = any($3::bigint[]) and status = 'open';
```

## Guardrails

- Never mark feedback resolved before the fix is committed.
- If the feedback is ambiguous, add diagnostic logging or a test that captures the current behavior before changing production logic.
- If a feedback cannot be reproduced from stored evaluation data, leave it open and write a concise note to the user instead of guessing.
- Commit messages must mention every resolved feedback ID.
