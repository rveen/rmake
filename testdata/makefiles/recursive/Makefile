SUBDIRS = lib app
.PHONY: all clean $(SUBDIRS)
all: $(SUBDIRS)
$(SUBDIRS):
	$(MAKE) -C $@
app: lib
clean:
	$(MAKE) -C lib clean
	$(MAKE) -C app clean
