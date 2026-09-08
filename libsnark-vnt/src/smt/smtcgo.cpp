#include <string>
#include <vector>
#include <cstring>
#include <mutex>

#include "libff/algebra/curves/alt_bn128/alt_bn128_pp.hpp"
#include "uint256.h"
#include "poseidon.hpp"
#include "poseidon_smt.hpp"
#include "smtcgo.hpp"

// Single global, process-wide Poseidon sparse Merkle tree (the public "state
// Merkle Tree"). All circuit libraries and Go call into this one instance via
// the C interface below, so there is exactly one authoritative tree per node.

using namespace poseidon;

namespace {
    PoseidonSMT* g_tree = nullptr;
    std::mutex g_mutex;

    PoseidonSMT& tree() {
        if (!g_tree) {
            libff::alt_bn128_pp::init_public_params();
            g_tree = new PoseidonSMT();
        }
        return *g_tree;
    }

    char* dupString(const std::string& s) {
        char* p = new char[s.size() + 1];
        std::memcpy(p, s.data(), s.size());
        p[s.size()] = '\0';
        return p;
    }
}

extern "C" {

// Insert a commitment (hex string, with or without 0x prefix) into the tree,
// setting its leaf (path = Poseidon(cmt)) to 1. Idempotent.
void smtInsertCMT(const char* cmt_hex) {
    std::lock_guard<std::mutex> lock(g_mutex);
    uint256 cmt = uint256S(cmt_hex);
    tree().insert(cmt);
}

// Return the current SMT root as a 64-char hex string (canonical big-endian
// field serialization, field_to_uint256_be). Caller frees with smtFree.
char* smtGetRoot() {
    std::lock_guard<std::mutex> lock(g_mutex);
    uint256 rt = tree().root_uint256();
    return dupString(rt.ToString());
}

// Reset the tree to empty (used for tests / fresh chains).
void smtReset() {
    std::lock_guard<std::mutex> lock(g_mutex);
    delete g_tree;
    g_tree = nullptr;
}

// Produce a membership proof for cmt_hex. Output layout (single string):
//   [0]            : '1' if found (leaf==1), '0' otherwise
//   [1 .. 256]     : path_bits, MSB-first, '0'/'1'
//   [257 .. 257+64*256-1] : 256 siblings, each 64 hex chars, leaf-first
//   [.. +64]       : root (64 hex chars)
// Total length = 1 + 256 + 256*64 + 64 = 16705 chars (+ null terminator).
// Caller frees with smtFree.
static char* proveSnapshot(PoseidonSMT& snapshot, const char* cmt_hex) {
    uint256 cmt = uint256S(cmt_hex);
    PoseidonSMT::Proof pr = snapshot.prove(cmt);

    std::string out;
    out.reserve(1 + 256 + 256 * 64 + 64);
    out.push_back(pr.found ? '1' : '0');
    for (size_t i = 0; i < 256; i++) out.push_back(pr.path_bits[i] ? '1' : '0');
    for (size_t i = 0; i < 256; i++) {
        std::string h = pr.siblings[i].ToString(); // 64 hex chars
        // ensure exactly 64 chars
        if (h.size() < 64) h = std::string(64 - h.size(), '0') + h;
        out += h;
    }
    {
        std::string h = pr.root.ToString();
        if (h.size() < 64) h = std::string(64 - h.size(), '0') + h;
        out += h;
    }
    return dupString(out);
}

char* smtProve(const char* cmt_hex) {
    std::lock_guard<std::mutex> lock(g_mutex);
    return proveSnapshot(tree(), cmt_hex);
}

void* smtSnapshotNew() {
    std::lock_guard<std::mutex> lock(g_mutex);
    tree(); // initialize curve parameters through the existing initialization path
    return new PoseidonSMT();
}
void* smtSnapshotClone(void* handle) { return new PoseidonSMT(*static_cast<PoseidonSMT*>(handle)); }
void smtSnapshotDelete(void* handle) { delete static_cast<PoseidonSMT*>(handle); }
void smtSnapshotInsert(void* handle, const char* cmt) { static_cast<PoseidonSMT*>(handle)->insert(uint256S(cmt)); }
char* smtSnapshotRoot(void* handle) { return dupString(static_cast<PoseidonSMT*>(handle)->root_uint256().ToString()); }
char* smtSnapshotProve(void* handle, const char* cmt) { return proveSnapshot(*static_cast<PoseidonSMT*>(handle), cmt); }

void smtFree(char* p) {
    delete[] p;
}

} // extern "C"
