cmake_minimum_required(VERSION 3.15)
project(mm_plugin_standalone VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

# See xenia-plugin/CMakeLists.txt's own comment on -fno-gnu-unique /
# hidden visibility / --exclude-libs,ALL -- same trap (a static-linked
# libstdc++ leaking STB_GNU_UNIQUE symbols into this .so's dynamic symbol
# table), same fix, reused verbatim here.
if(CMAKE_CXX_COMPILER_ID STREQUAL "GNU")
    add_compile_options($<$<COMPILE_LANGUAGE:CXX>:-fno-gnu-unique>)
endif()
set(CMAKE_CXX_VISIBILITY_PRESET hidden)
set(CMAKE_VISIBILITY_INLINES_HIDDEN ON)

find_package(Threads REQUIRED)

# Unlike xenia-plugin (which builds against charlesvestal/gearmulator, laid
# out as source/framework/<lib>), mdLib only exists in joelanders'
# gearmulator-md-mm fork, whose top-level source/CMakeLists.txt lays every
# shared library directly under source/ with no "framework/" prefix
# (source/baseLib, source/synthLib, source/hardwareLib, source/dsp56300/
# source, source/mc68k) -- confirmed by reading that fork's own
# source/CMakeLists.txt, not assumed from charlesvestal's layout.
if(NOT GEARMULATOR_MD_MM_SOURCE)
    set(GEARMULATOR_MD_MM_SOURCE ${CMAKE_CURRENT_SOURCE_DIR}/../../joelanders/gearmulator-md-mm/source)
endif()

# Same cache variables the top-level project sets before add_subdirectory
# (dsp56300/source) -- asmjit (the DSP56300 JIT backend) is vendored inside
# that submodule and expects these to already be set, not left at CMake's
# own defaults.
set(ASMJIT_STATIC TRUE)
set(ASMJIT_NO_INSTALL TRUE)
set(BUILD_SHARED_LIBS OFF)

add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/dsp56300/source ${CMAKE_BINARY_DIR}/dsp56300)
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/baseLib ${CMAKE_BINARY_DIR}/baseLib)
# libresample: mm-plugin itself never calls into it (Monomachine's native
# rate already matches this host's 44100Hz — see mm_plugin.cpp's header
# comment), but synthLib/CMakeLists.txt unconditionally links its
# "resample" target ("target_link_libraries(synthLib PUBLIC resample
# dsp56kBase baseLib)") for other synths' Device implementations, so the
# target must still exist to link synthLib at all. Confirmed by an actual
# build in this repo's dev session: omitting this subdirectory configures
# fine but fails at the final link with "cannot find -lresample".
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/libresample ${CMAKE_BINARY_DIR}/libresample)
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/synthLib ${CMAKE_BINARY_DIR}/synthLib)
# mc68k: Monomachine's own emulation is DSP56300-only (mdLib/CMakeLists.txt
# links only hardwareLib+dsp56kEmu, no 68kEmu) -- but hardwareLib itself is
# shared across every synth family in this fork and unconditionally links
# 68kEmu (hardwareLib/CMakeLists.txt: "target_link_libraries(hardwareLib
# PUBLIC 68kEmu synthLib)"), so the target must still exist to configure,
# even though no mdLib translation unit calls into it.
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/mc68k ${CMAKE_BINARY_DIR}/mc68k)
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/hardwareLib ${CMAKE_BINARY_DIR}/hardwareLib)
add_subdirectory(${GEARMULATOR_MD_MM_SOURCE}/elektron/md/mdLib ${CMAKE_BINARY_DIR}/mdLib)

add_library(mm-plugin SHARED mm_plugin.cpp)
# mdLib's own CMakeLists.txt does
# "target_include_directories(mdLib PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/..)"
# i.e. source/elektron -- so "#include mdLib/mddevice.h" (this project's
# own include style, matching gearmulator-md-mm's serverPlugin.cpp) resolves
# via that PUBLIC include dir once mm-plugin links mdLib, with no extra
# include path needed here for it. GEARMULATOR_MD_MM_SOURCE itself is still
# needed directly for synthLib/baseLib headers included by mdLib's own
# public headers.
target_include_directories(mm-plugin PRIVATE ${GEARMULATOR_MD_MM_SOURCE})
target_link_libraries(mm-plugin PRIVATE mdLib synthLib baseLib Threads::Threads)
target_link_options(mm-plugin PRIVATE -static-libstdc++ -static-libgcc -Wl,--exclude-libs,ALL)
