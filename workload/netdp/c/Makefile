# Workload eBPF datapath object, loaded by workloadagent (--netdp-obj).
CFLAGS=-O2 -g -Wall -Werror -target bpf -fno-stack-protector

all: netdp.o

netdp.o: netdp.c
	clang ${CFLAGS} -c -o $@ $<

.PHONY: all clean
clean:
	rm -f *.o
