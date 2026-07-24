# Build the paper from committed episode evidence.
# `generated/` and main.pdf are produced here, never hand-edited: the tables,
# macros, and figures come from `bench paper`, the PDF from tectonic (the
# repo's LaTeX engine; no pdflatex on the bench box).
#
# RUNS is the frozen evaluation grid (ADR 0004, tag eval-freeze-1). Adding a
# model round to the paper means adding its run dir here, on purpose.

REPO := $(shell git rev-parse --show-toplevel)
RUNS := \
	bench/results/20260722-161639 \
	bench/results/20260718-162855 \
	bench/results/20260718-162906

.PHONY: paper generated clean

paper: generated
	tectonic main.tex

# Regenerate results_table, summary_macros, and the pgfplots figures.
generated:
	cd $(REPO) && go run ./cmd/bench paper $(RUNS)

clean:
	rm -f main.pdf
