OBJS = a.o b.o
CFLAGS = -g
.PHONY: all
all: $(OBJS) gen

$(OBJS): %.o: %.c common.h
	$(CC) $(CFLAGS) -c $< -o $@ # stem $*

gen: gen.in
	cp $< $@; echo $* [$^] [$+] [$|]
