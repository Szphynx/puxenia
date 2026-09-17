cmake_minimum_required(VERSION 3.15)
project(xenia_plugin_standalone VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

# -fno-gnu-unique: glibc marks any DSO that DEFINES a unique symbol as
# NODELETE at relocation time, so dlclose() on it silently becomes a
# no-op -- schwung-je8086's own CMakeLists documents hitting this exact
# trap (254 STB_GNU_UNIQUE symbols, a whole debugging session before
# they found it) and states it must be set BEFORE the add_subdirectory()
# calls, since the unique symbols come from the vendored libs linked in,
# not from this file's own translation unit. Confirmed present here too:
# 212 STB_GNU_UNIQUE symbols and 8273 total exported symbols on a first
# build without these flags, almost entirely libstdc++ internals leaking
# out from the static link. Combined with default hidden visibility,
# only move_plugin_init_v2's explicit
# __attribute__((visibility("default"))) should remain exported.
if(CMAKE_CXX_COMPILER_ID STREQUAL "GNU")
    add_compile_options($<$<COMPILE_LANGUAGE:CXX>:-fno-gnu-unique>)
endif()
set(CMAKE_CXX_VISIBILITY_PRESET hidden)
set(CMAKE_VISIBILITY_INLINES_HIDDEN ON)

find_package(Threads REQUIRED)

if(NOT GEARMULATOR_SOURCE)
    set(GEARMULATOR_SOURCE ${CMAKE_CURRENT_SOURCE_DIR}/../../charlesvestal/gearmulator/source)
endif()

add_subdirectory(${GEARMULATOR_SOURCE}/cpu/dsp56300/source ${CMAKE_BINARY_DIR}/dsp56300)
add_subdirectory(${GEARMULATOR_SOURCE}/framework/baseLib ${CMAKE_BINARY_DIR}/baseLib)
add_subdirectory(${GEARMULATOR_SOURCE}/framework/synthLib ${CMAKE_BINARY_DIR}/synthLib)
add_subdirectory(${GEARMULATOR_SOURCE}/3rdparty/libresample ${CMAKE_BINARY_DIR}/libresample)
add_subdirectory(${GEARMULATOR_SOURCE}/framework/hardwareLib ${CMAKE_BINARY_DIR}/hardwareLib)
add_subdirectory(${GEARMULATOR_SOURCE}/cpu/mc68k ${CMAKE_BINARY_DIR}/mc68k)
target_include_directories(68kEmu PUBLIC ${GEARMULATOR_SOURCE}/cpu)
add_subdirectory(${GEARMULATOR_SOURCE}/waldi/common/wLib ${CMAKE_BINARY_DIR}/wLib)
add_subdirectory(${GEARMULATOR_SOURCE}/waldi/xt/xtLib ${CMAKE_BINARY_DIR}/xtLib)

add_library(xenia-plugin SHARED xenia_plugin.cpp)
target_include_directories(xenia-plugin PRIVATE ${GEARMULATOR_SOURCE} ${GEARMULATOR_SOURCE}/3rdparty/libresample/include)
target_link_libraries(xenia-plugin PRIVATE xtLib synthLib baseLib Threads::Threads)
# -Wl,--exclude-libs,ALL: -fno-gnu-unique only affects code THIS build
# compiles -- Ubuntu's own prebuilt libstdc++.a was compiled by Debian's
# toolchain without it, so pulling that archive in via -static-libstdc++
# still leaks its STB_GNU_UNIQUE symbols (confirmed: unchanged at 212
# even after -fno-gnu-unique + hidden visibility). --exclude-libs forces
# every symbol drawn from a statically-linked archive to stay local in
# the final .so regardless of how that archive itself was built --
# the actual fix for "static-link a library but don't let any of its
# symbols leak into this DSO's dynamic symbol table."
target_link_options(xenia-plugin PRIVATE -static-libstdc++ -static-libgcc -Wl,--exclude-libs,ALL)
