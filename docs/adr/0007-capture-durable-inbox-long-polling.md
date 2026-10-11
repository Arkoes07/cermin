# Capture saves before it thinks: a durable inbox fed by long polling

Every Telegram update is written as raw JSON to the `telegram_updates` table before anything else happens, and only then confirmed to Telegram. The handler works through that inbox in order:
1. It saves the Thought.
2. It asks the AI to classify it.
3. It replies once classification finishes, or after 8 seconds, whichever comes first. A late reply is just `✓ saved`, edited once the Kind and Topics arrive.

A catch-up loop finishes whatever the AI couldn't. This is how the PRD's "Capture never fails because the AI is slow or down" holds: a crash, an AI outage or a deploy leaves a row to retry, never a lost Thought. An update whose processing fails 5 times is skipped, with an alert to you, and kept in the inbox for a replay, so it never stalls the ones behind it.

Updates arrive by long polling: `getUpdates` with a timeout. For one bot this is as immediate as a webhook, because Telegram answers the held request the moment a message arrives. It also needs no inbound route.

## Considered Options

- **Webhook.** The better fit for many bots, high volume or scale-to-zero hosting, none of which applies here. It needs a public TLS route, a secret-header check, a `setWebhook` call per environment, and a tunnel for local development. Switching later is a small change, because both feed the same inbox.
- **Processing in memory, with no inbox.** Less code, but a crash between receiving and saving loses the Thought, and there is no raw record to replay after a parsing bug.
- **Acknowledging instantly, then classifying.** That means either two messages per Thought, or an instant reply with no Kind and Topics. Waiting up to 8 seconds replaced it, since a few seconds is acceptable.

## Consequences

- Two Telegram bots: one for production and one for development, because two pollers can't share a token.
- The inbox keeps raw text, but cuts a reply's or a pin's copy of another message down to its ID. `/delete` must therefore scrub the Thought's own inbox rows, its message and its edits, as well as the Thought.
