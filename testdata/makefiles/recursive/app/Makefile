LDLIBS = -L../lib -llib
app: main.o ../lib/liblib.a
	$(CC) -o $@ main.o $(LDLIBS)
clean:
	rm -f app *.o
