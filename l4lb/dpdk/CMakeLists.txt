cmake_minimum_required(VERSION 4.0)


project(quicl4lb_dpdk C)

set(CMAKE_C_STANDARD 23)

find_package(PkgConfig REQUIRED)
pkg_check_modules(DPDK REQUIRED libdpdk)


add_executable(quicl4lb_dpdk main.c)


target_include_directories(quicl4lb_dpdk PRIVATE ${DPDK_STATIC_INCLUDE_DIRS})
target_compile_options(quicl4lb_dpdk    PRIVATE ${DPDK_STATIC_CFLAGS_OTHER})

target_link_libraries(quicl4lb_dpdk PRIVATE ${DPDK_STATIC_LDFLAGS})
