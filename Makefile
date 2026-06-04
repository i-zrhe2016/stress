GO ?= go
BINARY := cpu_memory_stress

.PHONY: all clean

all: $(BINARY)

$(BINARY): cpu_memory_stress.go
	$(GO) build -o $@ $<

clean:
	rm -f $(BINARY)
