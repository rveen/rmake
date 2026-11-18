# Variable flavors, conditionals and functions.
A = $(B) late
B = early
C := $(A)
D ?= dval
D ?= ignored
E = one
E += two
F := x
F += $(F)y
override G = g1
G = g2
SRCS = src/a.c src/b.c lib/c.c
OBJS = $(SRCS:.c=.o)
OBJS2 = $(patsubst %.c,build/%.o,$(notdir $(SRCS)))
DIRS = $(sort $(dir $(SRCS)))
LIBSRC = $(filter lib/%,$(SRCS))
NOLIB = $(filter-out lib/%,$(SRCS))
UP = $(subst a,A,$(SRCS))
W = $(word 2,$(SRCS)) $(words $(SRCS)) $(firstword $(SRCS)) $(lastword $(SRCS)) $(wordlist 2,3,$(SRCS))
J = $(join a b,1 2) $(addprefix -I,$(DIRS)) $(addsuffix .bak,x y)
S = $(strip   a   b  ) $(findstring b,abc) $(basename a.b.c d) $(suffix a.c b)
IF = $(if $(B),yes,no) $(if ,yes,no) $(or ,,x) $(and a,b)
EACH = $(foreach f,$(SRCS),<$(f)>)
reverse = $(2) $(1)
CALL = $(call reverse,x,y)
ORIGIN = $(origin B) $(origin CC) $(origin NOPE) $(flavor A) $(flavor C)
ifeq ($(B),early)
COND = eq
else
COND = ne
endif
ifneq "$(B)" "early"
COND += bad
endif
ifdef B
COND += defined
endif
ifndef NOPE
COND += notdef
else ifeq (1,1)
COND += bad
endif
ESC = a\#b
LET = $(let x y,1 2 3,$(y)-$(x))

.PHONY: all
all:
	echo A=$(A) C=$(C) D=$(D) E=$(E) F=$(F) G=$(G)
	echo $(OBJS) $(OBJS2) $(DIRS) $(LIBSRC) $(NOLIB) $(UP)
	echo $(W) $(J) $(S) $(IF)
	echo $(EACH) $(CALL) $(ORIGIN) $(COND) $(ESC) $(LET)
	echo '$$HOME' "$$PATH" $$$$
