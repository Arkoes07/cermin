# Claude for language tasks, Gemini for embeddings, each behind one function

Cermin uses paid API tiers only, so no provider trains on Thoughts.
- **Classification** starts on Claude Sonnet 5.5 at low effort. That gives quality worth trusting while the daily habit forms, for about $1 to $1.50 a month. Ask, the Digest and the Portrait choose their Claude model in their own week.
- **Embeddings** come from Google's gemini-embedding-2 on the paid tier. Google reports it with the top multilingual MTEB score of the options considered, and it costs cents a year at this volume. Anthropic has no embedding model.

Each provider sits behind one function, `classify` or `embed`, and the model name is configuration. Moving between Claude models needs no code change. Adding another provider means writing one new implementation of that function.

## Considered Options

- **All OpenAI** (gpt-6-luna and text-embedding-3-large). One vendor, and token logprobs could give a calibrated Kind confidence. But its multilingual retrieval scores are weaker, and logprobs don't help with open-ended Topics.
- **All Gemini** (Flash-Lite and gemini-embedding-2). One vendor, but 3.x models return no logprobs.
- **Voyage embeddings.** Its terms reportedly train on inputs unless you opt out, which breaks the no-training rule.
- **Jev (TypeSafe), a fast classifier with calibrated confidence.** It fits choosing the Kind and choosing among existing Topics. But it is early access only, and publishes no data policy and no language support. It is evaluated in week 3 instead (see Consequences).
- **A provider abstraction now.** An interface with only one implementation is speculative. The seam gets formalised when a second classifier is evaluated in week 3.

## Consequences

- Claude exposes no token probabilities. Day-1 confidence is therefore the model's own stated confidence (method `verbalized-v1`), stored with each Assignment. It is expected to be poorly calibrated, and measuring how poorly is part of week 3.
- Two API accounts, two keys, two spend caps.
- In week 3, Claude Haiku 5.5, Sonnet 5.5, Opus 5.5 and Jev are compared on the hand-labelled set for accuracy and calibration. The reruns go through the Batch API at half price. The outcome is either the cheapest model that holds accuracy, or a cascade: a cheap classifier first, escalating to a stronger model when it is unsure or a new Topic is needed. Tracked in [#10](https://github.com/Arkoes07/cermin/issues/10).
- Changing the embedding model means re-embedding the Corpus, which costs cents. Embeddings are stored per model, so old and new can coexist during a comparison.
