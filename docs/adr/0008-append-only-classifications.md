# Append-only Classifications, with everything evaluation needs recorded from day 1

Week 3 has to measure accuracy and calibration. That depends on data that can't be reconstructed later: what Cermin first said, how sure it claimed to be, what it considered and dropped, and what you corrected. So the data model is shaped around evaluation:

- **Classifications are append-only.** Each one is a row that is never updated, whether it is Cermin's, a Correction, or a Declared Topic backfill. The latest is current, and Cermin's first is what accuracy is measured against.
- **Every candidate Topic is stored** with its stated confidence and whether it was kept. The strong-Topic threshold can then be tuned on history without re-running the model. Dropped candidates are stored by name only; a `topics` row appears only once a Topic is kept or given in a Correction. That way a name Cermin considered and dropped never enters the Topic list the classifier sees.
- **Each Classification records** the model, prompt version, confidence method and raw model output. The raw output includes the `kind_suggestion` the model gives when no Kind fits.
- **Raw input and timing are kept:**
  - every Telegram update, as raw JSON ([ADR-0007](0007-capture-durable-inbox-long-polling.md));
  - each Thought's Thought time and arrival time, separately;
  - the Telegram message IDs of the Thought, of the message it replied to, and of its Acknowledgement.
- **Kinds live in a `kinds` table**, seeded and changed only by migrations, and `classifications.kind` references it. **Topics live in a `topics` table** that grows as Cermin and you use new names.
- **`/delete` is a hard delete.** It cascades from the Thought and scrubs every inbox row that carries its text. Backups age out within 7 days.

## Considered Options

- **Update the current Kind and Topics in place and keep an audit log.** Reads are simpler, but Cermin's original answer would live only in a log that is easy to get wrong. With append-only rows, "first vs latest" is a plain query.
- **Store only the kept Topics.** Smaller, but week 3 would have to re-run the model to tune the threshold, and by then the model or prompt may have changed.
- **Kinds as a list in code.** One table fewer, but the database alone would neither enforce the set nor explain it to someone reading an evaluation copy.
- **Kinds that grow automatically, like Topics.** A Kind set that drifts:
  - breaks week-over-week comparisons in the Digest and the Portrait;
  - makes accuracy undefined;
  - and blurs shape back into subject, which the PRD deliberately separated.

  A runtime `/kind add` command is left as a v2 candidate.
- **A tombstone for deleted Thoughts.** It would keep old Insights' citations readable, but a deleted Thought should be really gone.

## Consequences

- Finding a Thought's current Kind and Topics means picking its latest Classification.
- Adding a Kind takes a migration plus a prompt-version bump, and accuracy is reported per prompt version.
- Storage grows by about 20 KB per Thought, around 40 MB a year.
