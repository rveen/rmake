SRCS := main.c util.c
OBJS := $(SRCS:%.c=build/%.o)
DEPS := $(OBJS:.o=.d)
CFLAGS += -MMD -MP

app: $(OBJS)
	$(CC) $^ -o $@

build/%.o: %.c | build
	$(CC) $(CFLAGS) -c $< -o $@

build:
	mkdir -p $@

-include $(DEPS)
