# Cermin: Product Requirements (v1)

Status: accepted, 2026-10-03. Milestones moved one week later on 2026-10-09. Cermin writes only English from 2026-10-15. Vocabulary: [CONTEXT.md](../../CONTEXT.md).

## Problem

I think a lot, around 5 Thoughts a day: business ideas, coding questions, lifestyle and philosophy. Good ones get forgotten. Notes don't fix this, because they stay unorganised, and getting anything out of them means rereading all of them by hand. I have no backlog: the Thoughts I've already had are lost, which is part of the pain.

Related pains, in priority order:

1. **Losing Thoughts.** A good idea comes back only by luck.
2. **Not seeing connections.** A Thought from today and one from last month belong together, and nothing points that out.
3. **Loops.** As an overthinker, I return to the same question again and again without noticing.
4. **Not knowing my own patterns.** What do I think about, when, and how is that changing?

## Goals

| | Goal | What it means |
|---|---|---|
| **G1** | A personal tool I actually use | Capturing takes seconds, and the Insights are worth reading. This is the north star. |
| **G2** | AI-engineering portfolio | Shows real AI work: classification, similarity, evaluation, MCP. |
| **G3** | Open source | A public repo (AGPL, [ADR-0001](../adr/0001-agpl-license.md)) and an MCP server. |

When goals conflict, G1 wins and G2 follows from it: a tool that's actually used, with real data, makes a better portfolio story than a demo.

## Non-goals (v1)

- **Multi-user, subscriptions, or a business.** Deferred behind the 4-week gate ([ADR-0002](../adr/0002-single-user-business-deferred.md)).
- **A custom app.** Capture happens in a chat app I already have.
- **Voice notes, images, links.** Text only. Voice is the likely v2 input.
- **Editing Thoughts.** The Corpus records what I thought at the time.
- **Personality typing** (MBTI, Big Five and similar). That would overreach from ~150 messages.
- **Therapy or mental-health features.** Loops are pointed out, never diagnosed or treated.
- **Outside contributions.** No external PRs until the gate is decided.

## User

One user: me. Mostly English or Bahasa Indonesia, occasionally mixed. Capture happens on my phone, often late at night.

## v1 scope

### Capture

- I send a Thought as a text message to a **Telegram bot**.
- Cermin replies **within a few seconds** with the Thought's Kind and Topics. Capture **never fails** because the AI is slow or down: the Thought is saved first, and classification and Insights catch up later.
- Thoughts are permanent. I can remove one by replying to it with `/delete`; there is no edit.
- **One-time import**: Thoughts captured in WhatsApp's "Message yourself" chat from 2026-10-03 until the bot is live are imported from a chat export, so the Corpus doesn't start empty.

### Classification

- Every Thought gets exactly one **Kind** (starting set: idea, question, observation, reflection, plan) and **1 to 3 Topics** (open-ended, emerging as the Corpus grows).
- **Every area a Thought is about, but no guesses.** Cermin keeps up to 3 Topics and drops only those unlikely to be right; if none is likely, it keeps the single best Topic. Cermin reuses an existing Topic rather than creating a near-duplicate.
- Both are shown compactly in the acknowledgement, e.g. `💡 idea · business, retail`.
- Cermin **stores** how confident it was in each assignment but **never shows** it in chat. Comparing stored confidence against my corrections shows whether "90% sure" really means right 90% of the time.
- I can correct Kind or Topics by replying. Corrections feed the evaluation set used to measure accuracy.
- **Kinds change only when I decide to.** Cermin never invents a Kind. When a Thought fits none of them well, it picks the closest one and notes the shape it would have named; those notes are the evidence for adding a Kind later. Corrections can only use existing Kinds.

The starting Kinds:

| Kind | Means | Example |
|---|---|---|
| 💡 idea | a possibility I could act on or build, not yet committed | "What if small shops could restock by chat?" |
| ❓ question | something I'm asking, of myself or the world, with no answer yet | "Why does Go still not have sum types?" |
| 👀 observation | something I noticed or learned about the world or other people | "Cafés near the office are full at 3pm." |
| 🪞 reflection | looking inward: feelings, values, lessons about myself | "I procrastinate more when a task is vague." |
| 📋 plan | something I intend or decided to do | "Start running on Saturdays." |

A mood is a reflection, a quote or a "TIL" is an observation, and a decision is a plan.

### Declared Topics

- `/topic add <name> <description>` declares a Topic in my own words: a project, a book, the job hunt. Running it again with the same name updates the description.
- Cermin applies it to new Thoughts by inferring from the name and the description. A `#name` written in the Thought is always respected. Declared Topics take priority within the 3-Topic limit.
- When I declare a Topic, Cermin **goes back and tags past Thoughts** that match, so its history doesn't start on the day I happened to declare it.
- If Cermin has already created a Topic with the same name, the declared one **takes it over**, keeping its Thoughts.
- `/topic archive <name>` stops new tagging and keeps the history.

### Resurfacing

- After a Thought is captured, Cermin replies within **about 10 seconds** with related past Thoughts, at most 2.
- It does this **only when the match is strong**: about the same specific subject, in any language, not merely sharing a Topic. Otherwise the acknowledgement is the whole reply. Showing a weak match every time would train me to ignore them.
- Thoughts I still remember never come back: those from the last 24 hours, and the one I replied to.
- A question and its later answer find each other this way. There is no open/closed tracking for questions.

### Reactions

- Every Insight message offers 👍 (useful) or 👎 (noise) as buttons under it. My latest tap counts, so a mis-tap can be fixed. The 👍 rate measures whether Insights are worth anything and whether the Resurfacing threshold is right.

### Ask

- `/ask <question>` answers from the Corpus, citing the Thoughts it used.
- Anything sent without `/ask` is a Thought, even if it's phrased as a question.

### MCP server

- Lets an AI assistant (Claude, for example) Ask the Corpus: *"what have I thought about starting a business?"*
- Declared Topics make it work in context: Claude Code, running in the Cermin repo, can ask *"what have I thought about Cermin?"*
- Read-only in v1: no Capture through MCP.

### Digest

- Sent every **Sunday morning**, in English.
- Covers the week's Thoughts by Kind and Topic, new Topics, active Loops, and the most 👍'd Resurfacings.

### Loops

- A Topic or question that keeps recurring across weeks is reported as a Loop, in the Digest and the Portrait.

### Portrait

- A **dated**, descriptive picture of how I think, built from the whole Corpus. Examples: "you mostly think in questions, not plans"; "philosophy shows up after 11pm"; "your Loops: career, Rust".
- **Every claim cites the Thoughts behind it.** Portraits from different dates can be compared to see how my thinking shifts.

## Language

- Cermin writes only English: Acknowledgements, Resurfacings, answers to an Ask, the Digest and the Portrait. Thoughts are quoted as written.
- Thoughts that mix English and Indonesian are accepted and need no special handling.

## Constraints

- **Budget:** $10/month or less for everything.
- **AI:** a hosted AI API for v1, chosen for quality on Bahasa Indonesia and for speed of shipping. Comparing it with a self-hosted model on my own evaluation set is a planned experiment, not a v1 requirement.
- **Privacy:** my Thoughts may go to the AI provider's API. The Corpus is **never public**.

## Success checks (end of week 4, 2026-11-08)

1. **The habit holds:** at least 3 Thoughts a day, on average, in week 4.
2. **Resurfacing works:** at least 5 Resurfacings rated 👍.
3. **Surprise:** at least 1 Insight told me something about myself I didn't know.
4. **Measured:** classification accuracy is reported against a hand-labelled set built mostly from my corrections.

**Business gate:** the business question is reopened only if checks 1–3 pass ([ADR-0002](../adr/0002-single-user-business-deferred.md)).

## Milestones

Weeks run Monday to Sunday.

| Week | Dates | Ships |
|---|---|---|
| 0 | 3–11 Oct | Product docs and technical design; start capturing in WhatsApp "Message yourself" |
| 1 | 12–18 Oct | Capture and classification live in Telegram; using Cermin daily begins |
| 2 | 19–25 Oct | Resurfacing, Reactions, WhatsApp import |
| 3 | 26 Oct – 1 Nov | Accuracy measured, `/ask`, Declared Topics, MCP server |
| 4 | 2–8 Nov | Digest, Loops, Portrait; gate review |

## Product decisions not worth an ADR

- **Telegram over WhatsApp.** WhatsApp's official API is free for replies sent within 24 hours of my last message. But since 15 Jan 2026, Meta's policy bans business accounts whose main purpose is an AI chatbot, and `/ask` sits in that grey area. Setup also needs a second phone number and a Meta business account, and unofficial libraries risk getting the number banned. Telegram bots are free, take minutes to set up, and carry no policy risk. WhatsApp stays a possible v2 channel if Telegram turns out to be the reason the habit fails.
- **Kind and Topic as two separate dimensions.** The original idea's list mixed shape ("question", "business idea") with subject ("coding", "lifestyle"), which made "a question about coding" impossible to classify.
- **No open/closed questions.** Closing questions is bookkeeping I wouldn't keep up. Resurfacing pairs an answer with its question, and a recurring question becomes a Loop.

- **Declared Topics instead of Projects.** The machinery a project needs (my exact name, a description, inferred tagging, going back over past Thoughts, archiving) works just as well for a book, a person or the job hunt. One general concept is better than a project-only one.
- **Topics sit at a single level, with no hierarchy.** A tree would force each Topic to have one parent ("pricing" under Cermin, or under business?) and would need upkeep. Up to 3 Topics per Thought gives the same grouping without either problem.

## v2 candidates

Considered, deliberately left out of v1, and worth revisiting when the trigger appears:

- **Voice notes**, if typing turns out to limit how often I capture.
- **WhatsApp capture**, if Telegram turns out to be why the habit fails.
- **`/topic merge`**, if the Digests show near-duplicate Topics piling up (e.g. coding / programming / software engineering).
- **Situation context**, meaning what I was doing when the Thought came (commuting, shower, gym), to support Portrait lines like "your business ideas come on the commute". Time of day stands in for it in v1.
- **Capture via MCP**, for saving a Thought from inside an AI assistant.

## Left for technical design

These aren't product decisions. They must satisfy the constraints above:

- Which model answers an Ask.
- How confidence is measured for Kind and Topic assignments (a language model's self-reported confidence is usually poorly calibrated), and the threshold for "strong" Topics.
- How Declared Topics are matched to Thoughts from their name and description.
- How Loops are detected.
