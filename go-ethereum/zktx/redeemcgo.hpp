#ifdef __cplusplus
extern "C"
{
#endif

#include <stdbool.h>
#include <stdint.h>

   char *genCMT(uint64_t value, char *sn_string, char *r_string);
   char* computePRF(char* sk_string, char* r_string);
   char *genRoot(char *cmtarray, int n);

   char *genRedeemproof(uint64_t value,
                      uint64_t value_old,
                      char *sn_old_string,
                      char *r_old_string,
                      char *sn_string,
                      char *r_string,
                      char *cmtA_old_string,
                      char *cmtA_string,
                      uint64_t value_s,
                      char *sk_string,
                      char *cmtarray,
                      int n,
                      char *RT
                      );

   bool verifyRedeemproof(char *data, char *sn_old_string, char *rt_string, char *cmtA_string, uint64_t value_s);

#ifdef __cplusplus
} // extern "C"
#endif
