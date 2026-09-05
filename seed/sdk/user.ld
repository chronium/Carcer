OUTPUT_FORMAT(elf64-x86-64)
OUTPUT_ARCH(i386:x86-64)
ENTRY(_start)
PHDRS {
    text PT_LOAD FLAGS(5);
    ro PT_LOAD FLAGS(4);
    data PT_LOAD FLAGS(6);
}
SECTIONS {
    . = 0x400000;
    .text : { KEEP(*(.text.start)) *(.text .text.*) } :text
    . = ALIGN(4096);
    .rodata : { *(.rodata .rodata.*) } :ro
    . = ALIGN(4096);
    .data : { *(.probe) *(.data .data.*) } :data
    .got : { *(.got .got.plt .igot.plt) } :data
    .unsupported : { *(.rela.* .rel.* .iplt) } :data
    ASSERT(SIZEOF(.got) == 0, "GOT is outside this SDK")
    ASSERT(SIZEOF(.unsupported) == 0, "runtime relocation is unsupported")
    .bss (NOLOAD) : { *(.bss .bss.*) *(COMMON) } :data
    /DISCARD/ : { *(.comment) *(.note*) *(.eh_frame*) }
}
