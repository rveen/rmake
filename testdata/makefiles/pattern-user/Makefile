.PHONY: all
all: build/x.o parser.o out/data.txt sub/y.o

build/%.o: src/%.c
	cc -c $< -o $@ [$*]

%.o: %.c
	cc -c $< -o $@ ($*) $(@D) $(@F) $(<D)

%.tab.c %.tab.h: %.y
	bison -d $< -o $*.tab.c

parser.o: parser.tab.h
parser.c: parser.tab.c
	cp $< $@

out/%.txt: %.raw
	tr a-z A-Z < $< > $@
