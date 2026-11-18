PROGS = foo bar
define PROG_template
$(1): $(1).o
	$$(CC) -o $$@ $$^ $$($(1)_LIBS)
endef
foo_LIBS = -lm
bar_LIBS =
$(foreach p,$(PROGS),$(eval $(call PROG_template,$(p))))

.PHONY: all
all: $(PROGS)
