# Resurfacing shortlists by raw-text embeddings, then Claude judges the shortlist

A Resurfacing should appear only when a past Thought is strongly related: about the same specific subject, in any language, not merely sharing a Topic ([CONTEXT.md](../../CONTEXT.md)). A weak match shown every time would train you to ignore all of them. Two choices decide what counts as strongly related:

- **Only the Thought's raw text is embedded.** It uses Qwen3-Embedding-0.6B on Workers AI at its 1024 dimensions, sent as a document, so with no task instruction. Kind and Topics stay out:
  - Topics are broad on purpose. Adding "coding" to "Go generics feel clunky" and to "Rust's borrow checker taught me ownership" pulls together exactly the pair that shouldn't come back. The specific words should decide.
  - Kind pushes the best pairs apart. A question and the observation that answers it have different Kinds.
  - The vector would have to wait for the classifier, which may be slow or down. Every Correction and every Declared Topic backfill would also mean embedding the Thought again.
  - Topics come from the same text, so they add little that the vector doesn't already have.
- **Cosine similarity shortlists the candidates, and a Claude Judge decides which are strongly related** ([#29](https://github.com/Arkoes07/cermin/issues/29)):
  - The shortlist is the eligible past Thoughts that score at least 0.45, at most 3, best first. 0.45 kept all 7 wanted pairs of #27's calibration.
  - One call to Claude Sonnet 5.5 per Thought, for its whole shortlist, states how confident it is, from 0 to 1, that each shortlisted Thought is strongly related to the new one. It sees the Thoughts' texts only, with no Kinds, Topics or dates, for the reasons above. Its prompt gives the definition from CONTEXT.md, the PRD's case of a question and its later answer, and made-up examples. It was written without reading the labelled pairs.
  - A shortlisted Thought is kept at a confidence of at least 0.6. A Resurfacing sends at most 2 kept Thoughts, those with the best cosine score.
  - The bar is precision: at least 8 in 10 of the pairs kept are wanted, as the PRD asks. A recall bar (at least half of the wanted pairs kept) was fixed with it before measuring, but no method reached both. You dropped recall from the bar for now: a Resurfacing that misses a wanted pair costs less than one you learn to ignore. Raising recall is [#30](https://github.com/Arkoes07/cermin/issues/30)'s work.
  - `cermin eval resurfacing` measured it on 53 labelled shortlisted pairs, 18 of them wanted. In one run it kept 8 pairs, 7 of them wanted: precision 0.88, recall 0.39. A second run kept 6, 5 of them wanted: 0.83 and 0.28. Its confidences vary from run to run, and 0.6 is the lowest threshold that passed in both runs.

## Considered Options

- **Embed the text together with its Kind and Topics.** Rejected for the reasons above. Vectors are stored per model, so "text + Topics" can still be tried as a variant if the labelled pairs show misses.
- **A relative threshold:** resurface when the best score stands out from the Thought's other scores. It needs no labels, but in a small Corpus something always stands out, so it would send pairs that only share a Topic.
- **A cosine cut-off alone:** the lowest score at which about 8 in 10 of the pairs at or above it are wanted. This was the first decision. On #27's 27 labelled pairs, each Thought with its best eligible earlier Thought, no cut-off reached 8 in 10. The highest-scoring pair was unwanted, and wanted and unwanted pairs overlapped from 0.47 to 0.70. On #29's 53 shortlisted pairs, the most precise cut-off, 0.6489, keeps 4 pairs, 3 of them wanted: precision 0.75, recall 0.17.
- **A Topic rule:** keep a shortlisted Thought that shares at least 2 current Topics with the new one. It needs no AI call. It was found on #27's pairs, where it keeps 6 pairs, all wanted, so only the 38 pairs labelled after it count. On those it keeps 12 pairs, 4 of them wanted: precision 0.33, recall 0.36.
- **The Judge and the Topic rule together,** keeping a pair only when both keep it. On the 38 held-out pairs, with the Judge at 0.5, Haiku 5.5 keeps none, and Sonnet 5.5 keeps 3, 2 of them wanted: precision 0.67, recall 0.18.
- **The Judge on Claude Haiku 5.5,** at about $0.02 a month instead of $0.35. At 0.5 it keeps 5 pairs, 4 of them wanted: precision 0.80, recall 0.22. Sonnet 5.5 at 0.6 did better on both in its first run.
- **Keep the pairs that share a Kind.** On #27's pairs, no unwanted pair at 0.45 or above shared a Kind. On #29's 53 pairs, 11 of the 18 wanted pairs share one, and so do 9 of the 35 unwanted ones, so a Kind alone doesn't separate them.
- **A fixed cut-off taken from published examples.** Cosine scores spread differently from one model, and one kind of text, to another, so a borrowed number is a guess.

## Consequences

- The floor, the cap and the keep confidence are constants in code, changed with a deploy. Every Resurfacing records its method, the embedding model, the floor and the cap, and the Judge's model, prompt version and keep confidence. Each citation keeps its cosine score and the Judge's confidence. Embeddings are stored, so another floor can be replayed over past Thoughts without an API call, but the Judge can't be replayed without calling it again.
- A Correction never changes a vector or what the Judge sees.
- Changing the embedding model invalidates the floor. Changing the Judge's model or prompt means measuring it again with `cermin eval resurfacing`.
- Each Thought with a shortlist adds a Claude call after the Acknowledgement: about 75 calls a month, about $0.35.
- Labelling takes about 15 minutes, on the laptop. The CSVs of real Thoughts stay in the gitignored `data/` folder until #30 has run on them, and are deleted then.
