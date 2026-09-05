/* Legacy launch supplies RDI=RSI=0. Argument launch supplies argc/argv.
 * Preserve them across stack alignment for the ordinary C main call. */
.section .text.start,"ax",@progbits
.global _start
.type _start,@function
_start:
    xor %ebp,%ebp
    cld
    and $-16,%rsp
    call main
    mov %eax,%edi
    xor %eax,%eax
    int $0x80
    ud2
.size _start,.-_start
.section .note.GNU-stack,"",@progbits
