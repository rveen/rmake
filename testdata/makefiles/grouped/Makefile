.PHONY: all
all: a b c

a b &: in.txt
	gen $< a b

c: a
	cat a > c
