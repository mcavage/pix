---
description: Split a task into independent slices and run them as parallel subagents
argument-hint: "<task or question>"
---
Break the following work into independent, non-overlapping slices, then use the
`subagent` tool in parallel mode (one `fanout` agent per slice) to run them at
once. Give each slice a tight, self-contained task. When they return, synthesize
the results into one answer and call out any conflicts between slices.

Work: $ARGUMENTS
