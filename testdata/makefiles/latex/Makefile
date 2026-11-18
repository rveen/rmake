DOC = paper
TEX = pdflatex -interaction=nonstopmode
BIB = bibtex
FIGS = $(wildcard figs/*.svg)
PDFS = $(FIGS:.svg=.pdf)

$(DOC).pdf: $(DOC).tex refs.bib $(PDFS)
	$(TEX) $(DOC)
	$(BIB) $(DOC)
	$(TEX) $(DOC)
	$(TEX) $(DOC)

figs/%.pdf: figs/%.svg
	inkscape --export-filename=$@ $<

clean:
	rm -f *.aux *.log *.bbl *.blg $(DOC).pdf $(PDFS)
.PHONY: clean
