# Reading the rule book

`mongo-log-parser labels` prints every label definition; `--json` gives the same as data. A label
MongoDB renamed between versions has one definition per version range, and the node's version picks
it.

| Field | Meaning |
| --- | --- |
| signal | What one second's value is: FTDC paths summed, divided, scaled. `Δ` is a counter read as its change a second. |
| read as | Which summary of each minute the bands and the shape use: the minute's mean (a ratio of sums for a ratio), its highest or lowest second, or the seconds past a threshold. |
| levels | Fuzzy bands, lowest first. `low ≤25 (fading to 50)` is fully low up to 25 and partly low up to 50; a band `moderate 40–65 (edges 25, 80)` rises from 25, is full from 40 to 65, and is gone at 80. |
| mark | The duty cycle counts the minutes at or past it. A label past its mark in more than a rare minute is worth opening. |
| change | The smallest move that counts as a step, an episode or a trend. |
| pairs | The labels read over the same seconds when this one is opened. |

## Shapes

- `steady`, `fluctuating`, `constant`: nothing changed in a way that matters.
- `spiky`: excursions under three minutes.
- `episodic`: three minutes or more clearly above the label's own usual level, then back (or still
  there at the end, marked `+`).
- `periodic`: excursions at a steady interval — a job, not an accident.
- `step_up`, `step_down`: one level, then another that stayed.
- `rising`, `falling`: one way across the whole window.

## Tiers on the report

- **Worth opening**: the worst minute reached a band the rule calls worth opening (the top two of
  four or more by default; a rule can lower it — a single second with no ticket left is enough), or
  the label spent more than a rare minute past its mark, or a notable shape above its lowest band.
  A rule can also name labels that must agree (`open_with`): a disk busy every second is held back
  to "worth noticing" when its await stayed fast, and the row says so.
- **Worth noticing**: above its lowest band, or a notable shape on a label without bands.
- **Ordinary**: counted per node, every one in `--json`.
