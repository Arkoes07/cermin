-- Numbers about your use of Cermin (design doc §5.4). Read-only.
--
-- Production: fly ssh console -C "sqlite3 -readonly -box /data/cermin.db", then paste a query.
-- Locally:    sqlite3 -readonly -box data/cermin.db < docs/ops/queries.sql
--
-- Times are stored in UTC; '+7 hours' reads them in WIB. Week 2 adds success
-- check 2 (👍 Resurfacings) once Reactions are stored; check 4, accuracy,
-- comes from `cermin eval` in week 3.

-- Thoughts per day, last 14 days.
SELECT date(thought_at, '+7 hours') AS day, count(*) AS thoughts
FROM thoughts
WHERE thought_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-14 days')
GROUP BY day
ORDER BY day;

-- Success check 1: at least 3 Thoughts a day, on average, in week 4 (2–8 Nov).
SELECT round(count(*) / 7.0, 1) AS thoughts_per_day
FROM thoughts
WHERE date(thought_at, '+7 hours') BETWEEN '2026-11-02' AND '2026-11-08';

-- Acknowledgements sent as ✓ saved, last 14 days: Captures that Cermin
-- classified more than 8 s after they arrived, or hasn't classified yet.
SELECT count(*) AS captures,
       sum(c.id IS NULL OR (julianday(c.created_at) - julianday(t.received_at)) * 86400 > 8) AS sent_pending
FROM thoughts t
LEFT JOIN classifications c ON c.thought_id = t.id AND c.source = 'cermin'
WHERE t.source = 'telegram'
  AND t.received_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-14 days');

-- Corrections, last 14 days: Thoughts Cermin classified, and how many of them
-- you corrected. A rough signal until week 3 measures accuracy.
SELECT count(*) AS classified,
       sum(EXISTS (
           SELECT 1 FROM classifications x WHERE x.thought_id = c.thought_id AND x.source = 'correction'
       )) AS corrected
FROM classifications c
WHERE c.source = 'cermin'
  AND c.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-14 days');
