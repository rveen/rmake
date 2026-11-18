.SUFFIXES:
.SUFFIXES: .txt .html .gz

.txt.html:
	pandoc $< -o $@

.html.gz:
	gzip -c $< > $@

all: index.gz about.html
