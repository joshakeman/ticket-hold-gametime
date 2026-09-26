---
name: check-comments
description: Read-only review of code comments and docs for accuracy. Flags comments that contradict the code, overclaim what it does, or restate the obvious, with file:line, proof and a suggested rewrite. Reviews the current git diff, or the files named as arguments.
context: fork
disable-model-invocation: true
---

Adapted from Ardan Labs' service repo (github.com/ardanlabs/service), Apache License 2.0.

# Check comments

Read every comment as a claim about the code, and check that the code keeps it.
This is advisory only: never edit, stage or commit anything.

**Scope:** the files named in `$ARGUMENTS` if any; otherwise the comments added
or changed in `git diff` and `git diff --cached`. Also read the surrounding,
unchanged code each comment describes. If there's no diff and no arguments,
say so and stop.

## Steps

1. **List the claims.** For each comment, write down what it asserts: inputs,
   outputs, errors, side effects, ordering, locking or concurrency, invariants,
   which function or statement is responsible.
2. **Check each claim against the code.** Put particular weight on claims about
   *which line does the work*. In this repo, for example, the lock order comes
   from `ORDER BY id FOR UPDATE` in SQL, not from sorting a slice in Go. A
   comment that credits the wrong mechanism is a real finding even when the
   overall behavior is correct.
3. **Decide:** keep it, rewrite it (stale, overclaiming, or credits the wrong
   code), or remove it (narrates what the code plainly says).

## Severity

- **Stop:** a misleading comment could lead someone to break an invariant,
  such as double-selling a seat, adding a deadlock, or dropping idempotency.
- **Fix:** a doc comment on an exported or central function contradicts its behavior.
- **Tighten:** non-obvious behavior or an invariant is missing context.
- **Polish:** redundant or wordy.

## Output

```
Scope: <files or "git diff">
Overall: <highest severity found, or Clear>

### <Severity>: <short title>
- Where: path/file.go:line
- Comment says: <claim>
- Code does: <what actually happens, citing the line that proves it>
- Suggested wording: <replacement, or "remove">

Accurate comments worth keeping: <short list>
Needs confirmation: <claims you couldn't verify from the code>
```
