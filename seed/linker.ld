OUTPUT_FORMAT(elf64-x86-64)
ENTRY(kmain)
PHDRS {
text PT_LOAD FLAGS(5);
rodata PT_LOAD FLAGS(4);
data PT_LOAD FLAGS(6);
}
SECTIONS {
. = 0xffffffff80000000;
.text : ALIGN(4096) { *(.text .text.*) } :text
.rodata : ALIGN(4096) { *(.rodata .rodata.*) } :rodata
.data : ALIGN(4096) { *(.data .data.*) } :data
/* 1 MiB source content plus embedded-file metadata and ordinary kernel data. */
ASSERT(SIZEOF(.data) <= 2M, "embedded data exceeds guest linker budget")
.limine_requests : ALIGN(16) {
KEEP(*(.limine_requests_start))
KEEP(*(.limine_requests))
KEEP(*(.limine_requests_end))
} :data
.bss : ALIGN(16) { *(.bss .bss.*) *(COMMON) } :data
/DISCARD/ : { *(.comment) *(.note .note.*) *(.eh_frame .eh_frame.*) }
}
