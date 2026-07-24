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

.PHONY: paper generated arxiv clean

paper: generated
	tectonic main.tex

# Regenerate results_table, summary_macros, and the pgfplots figures.
generated:
	cd $(REPO) && go run ./cmd/bench paper $(RUNS)

# arXiv submission bundle. arXiv compiles the TeX source itself and does
# not run BibTeX, so the bundle carries main.bbl instead of references.bib.
# The generated/ inputs ship inside the tarball at the same relative path
# main.tex \inputs them from. Verified by a standalone compile in a temp dir.
arxiv: generated
	tectonic --keep-intermediates main.tex
	tar czf arxiv.tar.gz main.tex main.bbl generated/*.tex
	@dir=$$(mktemp -d) && tar xzf arxiv.tar.gz -C $$dir && \
		(cd $$dir && tectonic main.tex >/dev/null 2>&1) && \
		echo "arxiv.tar.gz OK: standalone compile passed" || \
		{ echo "arxiv.tar.gz FAILED standalone compile"; exit 1; }

clean:
	rm -f main.pdf main.bbl main.aux main.log arxiv.tar.gz
