
#include <errno.h>
#include <rte_config.h>
#include <rte_mempool.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "fcntl.h"
#include "unistd.h"
#include <rte_eal.h>
#include <rte_errno.h>
#include <rte_mbuf.h>
#include <rte_ethdev.h>
#include <rte_malloc.h>

const char *drivers_path = "/sys/bus/pci/drivers";
const char *devices_path = "/sys/bus/pci/devices";
const char* driver_name = "uio_pci_generic";

const char* driver_unbind =  "/sys/bus/pci/devices/01:00.0/driver/unbind";
const char* driver_bind = "/sys/bus/pci/drivers/uio_pci_generic/bind";
const char* pci_number = "01:00.0";

int write_to_device(const char* path) {
    int driver =  open(driver_bind,O_WRONLY,0);
    if (driver < 0) {
        perror("open driver_bind");
        return EXIT_FAILURE;
    }
    int result = write(driver,pci_number,strlen(pci_number));
    if (result < 0) {
        perror("write pci_number");
        return EXIT_FAILURE;
    }
    close(driver);
    return EXIT_SUCCESS;
}
#define MEMPOOL_CACHE_SIZE 256

sig_atomic_t fired;

void do_signal(int sig) {
    fired = sig;
}

int report_error(const char* msg,...) {
    va_list args;
    va_start(args, msg);
    vfprintf(stderr, msg, args);
    va_end(args);
    return EXIT_FAILURE;
}
#define MAX_PKT_BURST      32
#define MEMPOOL_CACHE_SIZE 256
    struct rte_eth_dev_tx_buffer *tx_buffer[RTE_MAX_ETHPORTS][RTE_MAX_QUEUES_PER_PORT];
    struct rte_mbuf *pkts_burst[MAX_PKT_BURST];

int main() {
    fired = 0;
    signal(SIGINT, do_signal);
    signal(SIGTERM,do_signal);
    if(write_to_device(driver_unbind) != EXIT_SUCCESS)  {
        return EXIT_FAILURE;
    }
     if(write_to_device(driver_bind) != EXIT_SUCCESS)  {
        return EXIT_FAILURE;
    }
    char* argv[] = {
        "ql4lb",
        "-l",
        "0",
        NULL,
    };
    const int argc = sizeof(argv) / sizeof(argv[0]) - 1;
    if(rte_eal_init(argc,argv) < 0) {
        errno = rte_errno;
        perror("rte_eal_init");
        return EXIT_FAILURE;
    }

      uint16_t nb_rxd = 1024;
  uint16_t nb_txd = 1024;

    uint16_t nb_lcores = 4;
    uint16_t nb_ports =  rte_eth_dev_count_avail ();



  unsigned int nb_mbufs = RTE_MAX (nb_ports * (nb_rxd + nb_txd + MAX_PKT_BURST +
                                  nb_lcores * MEMPOOL_CACHE_SIZE),
                      8192U);

  struct  rte_mempool* mempool = rte_pktmbuf_pool_create ("quic-lb", nb_mbufs, MEMPOOL_CACHE_SIZE, 0,
                               9500U, rte_socket_id ());

    if(mempool == NULL) {
        perror("mempool init");
        return EXIT_FAILURE;
    }



    uint16_t port_id = 0 ,queue_id = 0;

    struct rte_eth_rxconf rxq_conf;
    struct rte_eth_txconf txq_conf;
    uint16_t nb_rx_queue = 1;
    uint16_t nb_tx_queue = 1;

    struct rte_eth_conf port_conf =
    { .txmode = { .mq_mode = RTE_ETH_MQ_TX_NONE, },
    };

    int  ret = rte_eth_dev_configure (port_id, nb_rx_queue, nb_tx_queue,&port_conf);
    if (ret < 0) {
        return report_error("rte_eth_dev_configure(): port: %d failed: %d\n", port_id, ret); 
    }

    ret = rte_eth_rx_queue_setup (port_id, queue_id, nb_rxd,
                                rte_eth_dev_socket_id (port_id),
                                &rxq_conf, mempool);
    if(ret < 0) {
        return report_error("rte_eth_rx_queue_setup(): port: %d failed: %d\n", port_id, ret); 
    }

    ret = rte_eth_tx_queue_setup (
              port_id, queue_id, nb_txd, rte_eth_dev_socket_id (port_id), &txq_conf);
    if (ret < 0) {
        return report_error("rte_eth_tx_queue_setup(): port: %d failed: %d\n", port_id, ret); 
    }

    tx_buffer[port_id][queue_id] = rte_zmalloc_socket (
              "tx_buffer", RTE_ETH_TX_BUFFER_SIZE (MAX_PKT_BURST), 0,
              rte_eth_dev_socket_id (port_id));

    while (fired == 0) {
        uint16_t nb_rx = rte_eth_rx_burst(port_id,queue_id, pkts_burst, MAX_PKT_BURST);
        for(int i = 0; i < nb_rx; i++) {
            rte_eth_tx_buffer(port_id, queue_id, tx_buffer[port_id][queue_id], pkts_burst[i]);
        }
        rte_eth_tx_buffer_flush(port_id, queue_id, tx_buffer[port_id][queue_id]);
    }
}