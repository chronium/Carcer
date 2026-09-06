#ifndef CXLIB_H
#define CXLIB_H
#include <stddef.h>
#include <stdint.h>
#include <stdarg.h>
#include <limits.h>
#include "../cx.h"
#define EOF (-1)
#define SEEK_SET 0
#define SEEK_CUR 1
#define SEEK_END 2
#define EXIT_SUCCESS 0
#define EXIT_FAILURE 1
#define RAND_MAX 2147483647
#define EINVAL 22
#define ENOMEM 12
#define ENOENT 2
#define EACCES 13
#define EIO 5
#define ERANGE 34
#define ENOSYS 38
#define EEXIST 17
#define EISDIR 21
extern int errno;
typedef struct cx_stream FILE;
extern FILE *stdin,*stdout,*stderr;
void *malloc(size_t);void free(void *);void *calloc(size_t,size_t);
void *realloc(void *,size_t);
char *strcpy(char *,const char *);char *strncpy(char *,const char *,size_t);
char *strcat(char *,const char *);char *strncat(char *,const char *,size_t);
int strcmp(const char *,const char *);int strncmp(const char *,const char *,size_t);
int strcasecmp(const char *,const char *);int strncasecmp(const char *,const char *,size_t);
char *strchr(const char *,int);char *strrchr(const char *,int);
char *strstr(const char *,const char *);char *strdup(const char *);
int isspace(int);int isdigit(int);int isalpha(int);int isalnum(int);
int isupper(int);int islower(int);int isprint(int);int isxdigit(int);
int toupper(int);int tolower(int);
long strtol(const char *,char **,int);
unsigned long strtoul(const char *,char **,int);
double strtod(const char *,char **);
int atoi(const char *);long atol(const char *);double atof(const char *);
int abs(int);long labs(long);double fabs(double);
char *getenv(const char *);int system(const char *);
__attribute__((noreturn)) void exit(int);
__attribute__((noreturn)) void abort(void);
int atexit(void (*)(void));
int mkdir(const char *,unsigned);
int remove(const char *);int rename(const char *,const char *);
FILE *fopen(const char *,const char *);int fclose(FILE *);int fflush(FILE *);
size_t fread(void *,size_t,size_t,FILE *);
size_t fwrite(const void *,size_t,size_t,FILE *);
int fseek(FILE *,long,int);long ftell(FILE *);void rewind(FILE *);
int fgetc(FILE *);int fputc(int,FILE *);char *fgets(char *,int,FILE *);
int fputs(const char *,FILE *);int puts(const char *);int putchar(int);
int feof(FILE *);int ferror(FILE *);void clearerr(FILE *);
int vsnprintf(char *,size_t,const char *,va_list);
int snprintf(char *,size_t,const char *,...);
int vsprintf(char *,const char *,va_list);int sprintf(char *,const char *,...);
int vfprintf(FILE *,const char *,va_list);int fprintf(FILE *,const char *,...);
int vprintf(const char *,va_list);int printf(const char *,...);
int vsscanf(const char *,const char *,va_list);int sscanf(const char *,const char *,...);
/* Explicit path-backed standard streams. No process-global console exists.
 * NULL paths leave that stream unchanged. Returns -1 on failure. */
int cx_stdio(const char *out,const char *err);
#endif
