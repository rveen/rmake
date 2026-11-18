.PHONY: all clean install
all: version.h prog

version.h: FORCE
	echo '#define V 1' > $@

FORCE:

prog: main.c version.h
	cc -o $@ main.c

install: all
	install -m 755 prog /usr/local/bin/prog

clean:
	-rm -f prog version.h
