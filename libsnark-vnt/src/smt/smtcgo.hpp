#ifndef SMTCGO_HPP_
#define SMTCGO_HPP_

#ifdef __cplusplus
extern "C" {
#endif

void smtInsertCMT(const char* cmt_hex);
char* smtGetRoot();
void smtReset();
char* smtProve(const char* cmt_hex);
void smtFree(char* p);
// Isolated snapshots. Caller serializes access and owns the returned handle.
void* smtSnapshotNew();
void* smtSnapshotClone(void* handle);
void smtSnapshotDelete(void* handle);
void smtSnapshotInsert(void* handle, const char* cmt);
char* smtSnapshotRoot(void* handle);
char* smtSnapshotProve(void* handle, const char* cmt);

#ifdef __cplusplus
}
#endif

#endif // SMTCGO_HPP_
