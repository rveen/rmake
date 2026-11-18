CFLAGS = -O2 -MMD
.PHONY: all docs clean
all: liblib.a docs
liblib.a: a.o b.o
	$(AR) rcs $@ $^
docs:
	$(MAKE) -C doc
clean:
	rm -f *.o *.d liblib.a
-include *.d
