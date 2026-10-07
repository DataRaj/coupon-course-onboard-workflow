Public observation: 2026-10-06, anonymous Playwright Chromium navigation to
https://www.pw.live/iit-jee/class-12/batches/lakshya-jee-in-english-2027-348091.

`batch.json` retains only catalog fields from the rendered page's React flight
batch-detail props. Internal flags, account fields, unrelated content and teacher
contact/profile data were removed. `flight.txt` packages these same sanitized
props in representative flight records, including a length-prefixed text record.
`dom.json` contains selected rendered public sections and the explicitly struck
original price. These fixtures are observations, not authoritative current prices.

Network tests wrap the observed public batch fields in a synthetic JSON response
envelope. The live observation used embedded state, not an XHR response; tests do
not claim that envelope was captured from a live endpoint. SEO JSON-LD advertised
a different price and is deliberately not accepted as a commercial source.
