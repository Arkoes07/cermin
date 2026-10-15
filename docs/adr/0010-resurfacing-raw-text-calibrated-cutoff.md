# Resurfacing compares raw-text embeddings against a cut-off calibrated on labelled pairs

A Resurfacing should appear only when a past Thought is strongly related: about the same specific subject, in any language, not merely sharing a Topic ([CONTEXT.md](../../CONTEXT.md)). A weak match shown every time would train you to ignore all of them. Two choices decide what counts as strongly related:

- **Only the Thought's raw text is embedded.** It uses gemini-embedding-2 at its full 3072 dimensions, with no task instruction. Kind and Topics stay out:
  - Topics are broad on purpose. Adding "coding" to "Go generics feel clunky" and to "Rust's borrow checker taught me ownership" pulls together exactly the pair that shouldn't come back. The specific words should decide.
  - Kind pushes the best pairs apart. A question and the observation that answers it have different Kinds.
  - The vector would have to wait for the classifier, which may be slow or down. Every Correction and every Declared Topic backfill would also mean embedding the Thought again.
  - Topics come from the same text, so they add little that the vector doesn't already have.
- **"Strong" means the cosine similarity reaches a cut-off calibrated on your own labelled pairs.** No published number fits short, bilingual Thoughts on this model. The calibration runs once the WhatsApp import has filled the Corpus:
  - On a copy restored from R2, `cermin pairs` lists each Thought's best eligible earlier Thought, sorted by score.
  - You mark each pair, from the top down, as one you would want to see or not.
  - The cut-off is the lowest score at which about 8 in 10 of the pairs at or above it are wanted. That favours precision, as the PRD asks. Reactions retune it later.

## Considered Options

- **Embed the text together with its Kind and Topics.** Rejected for the reasons above. Vectors are stored per model, so "text + Topics" can still be tried as a variant if the labelled pairs show misses.
- **A relative threshold:** resurface when the best score stands out from the Thought's other scores. It needs no labels, but in a small Corpus something always stands out, so it would send pairs that only share a Topic.
- **A shortlist by cosine similarity, then Claude judges each candidate against the definition of strong.** This is best at telling "same subject" from "same Topic". It costs a Claude call per Thought (cents a month on Haiku) and adds 1–3 s of latency. It is the next step: it gets added if no cut-off reaches 8 in 10 without also dropping most of the wanted pairs.
- **A fixed cut-off taken from published examples.** Cosine scores spread differently from one model, and one kind of text, to another, so a borrowed number is a guess.

## Consequences

- The cut-off is a constant in code, changed with a deploy, and every Resurfacing records the cut-off it used. Embeddings are stored, so any other cut-off can be replayed over past Thoughts without an API call.
- A Correction never changes a vector.
- Changing the embedding model invalidates the cut-off, so it has to be calibrated again on the new model's vectors.
- Calibration takes about 15 minutes of your labelling, done on the laptop. The CSV of real Thoughts stays in the gitignored `data/` folder and is deleted afterwards.
