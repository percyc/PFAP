#ifdef __cplusplus
extern "C"
{
#endif

#include <stdbool.h>
#include <stdint.h>

   char *genCMT(uint64_t value, char *sn_string, char *r_string);
   char* computePRF(char* sk_string, char* r_string);

   char *genCreateAccountproof(char *sk_string,
                               char *r_A_string,
                               char *sn_A_string,
                               char *cmtA_string
                               );

   bool verifyCreateAccountproof(char *data, char *cmtA_string);

#ifdef __cplusplus
}
#endif
