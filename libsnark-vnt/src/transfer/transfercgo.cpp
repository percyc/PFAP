#include <stdio.h>
#include <iostream>

#include<sys/time.h>

#include <boost/optional.hpp>
#include <boost/foreach.hpp>
#include <boost/format.hpp>
#include <boost/array.hpp>

#include "libsnark/zk_proof_systems/ppzksnark/r1cs_gg_ppzksnark/r1cs_gg_ppzksnark.hpp"
#include "libsnark/common/default_types/r1cs_gg_ppzksnark_pp.hpp"
#include <libsnark/gadgetlib1/gadgets/hashes/sha256/sha256_gadget.hpp>
#include "libff/algebra/curves/alt_bn128/alt_bn128_pp.hpp"
#include "libsnark/gadgetlib1/gadgets/merkle_tree/merkle_tree_check_read_gadget.hpp"

#include "Note.h"
#include "uint256.h"
#include "transfercgo.hpp"
#include "IncrementalMerkleTree.hpp"

using namespace libsnark;
using namespace libff;
using namespace std;
using namespace libvnt;

#include "circuit/gadget.tcc"

int convertFromAscii(uint8_t ch)
{
    if (ch >= '0' && ch <= '9')
    {
        return ch - '0';
    }
    else if (ch >= 'a' && ch <= 'f')
    {
        return ch - 'a' + 10;
    }
}

libff::bigint<libff::alt_bn128_r_limbs> libsnarkBigintFromBytes(const uint8_t *_x)
{
    libff::bigint<libff::alt_bn128_r_limbs> x;

    for (unsigned i = 0; i < 4; i++)
    {
        for (unsigned j = 0; j < 8; j++)
        {
            x.data[3 - i] |= uint64_t(_x[i * 8 + j]) << (8 * (7 - j));
        }
    }
    return x;
}

template <typename T>
void writeToFile(std::string path, T &obj)
{
    std::stringstream ss;
    ss << obj;
    std::ofstream fh;
    fh.open(path, std::ios::binary);
    ss.rdbuf()->pubseekpos(0, std::ios_base::out);
    fh << ss.rdbuf();
    fh.flush();
    fh.close();
}

template <typename T>
T loadFromFile(std::string path)
{
    std::stringstream ss;
    std::ifstream fh(path, std::ios::binary);

    assert(fh.is_open());

    ss << fh.rdbuf();
    fh.close();

    ss.rdbuf()->pubseekpos(0, std::ios_base::in);

    T obj;
    ss >> obj;

    return obj;
}

void serializeProvingKeyToFile(r1cs_gg_ppzksnark_proving_key<alt_bn128_pp> pk, const char *pk_path)
{
    writeToFile(pk_path, pk);
}

void vkToFile(r1cs_gg_ppzksnark_verification_key<alt_bn128_pp> vk, const char *vk_path)
{
    writeToFile(vk_path, vk);
}

void proofToFile(r1cs_gg_ppzksnark_proof<alt_bn128_pp> pro, const char *pro_path)
{
    writeToFile(pro_path, pro);
}

r1cs_gg_ppzksnark_proving_key<alt_bn128_pp> deserializeProvingKeyFromFile(const char *pk_path)
{
    return loadFromFile<r1cs_gg_ppzksnark_proving_key<alt_bn128_pp>>(pk_path);
}

r1cs_gg_ppzksnark_verification_key<alt_bn128_pp> deserializevkFromFile(const char *vk_path)
{
    return loadFromFile<r1cs_gg_ppzksnark_verification_key<alt_bn128_pp>>(vk_path);
}

r1cs_gg_ppzksnark_proof<alt_bn128_pp> deserializeproofFromFile(const char *pro_path)
{
    return loadFromFile<r1cs_gg_ppzksnark_proof<alt_bn128_pp>>(pro_path);
}

std::string HexStringFromLibsnarkBigint(libff::bigint<libff::alt_bn128_r_limbs> _x)
{
    uint8_t x[32];
    for (unsigned i = 0; i < 4; i++)
        for (unsigned j = 0; j < 8; j++)
            x[i * 8 + j] = uint8_t(uint64_t(_x.data[3 - i]) >> (8 * (7 - j)));

    std::stringstream ss;
    ss << std::setfill('0');
    for (unsigned i = 0; i < 32; i++)
    {
        ss << std::hex << std::setw(2) << (int)x[i];
    }

    std::string str = ss.str();
    return str.erase(0, min(str.find_first_not_of('0'), str.size() - 1));
}

std::string outputPointG1AffineAsHex(libff::alt_bn128_G1 _p)
{
    libff::alt_bn128_G1 aff = _p;
    aff.to_affine_coordinates();

    std::string s_x = HexStringFromLibsnarkBigint(aff.X.as_bigint());
    while (s_x.size() < 64)
    {
        s_x = "0" + s_x;
    }

    std::string s_y = HexStringFromLibsnarkBigint(aff.Y.as_bigint());
    while (s_y.size() < 64)
    {
        s_y = "0" + s_y;
    }
    return s_x + s_y;
}

std::string outputPointG2AffineAsHex(libff::alt_bn128_G2 _p)
{
    libff::alt_bn128_G2 aff = _p;
    aff.to_affine_coordinates();

    std::string x_1 = HexStringFromLibsnarkBigint(aff.X.c1.as_bigint());
    while (x_1.size() < 64)
    {
        x_1 = "0" + x_1;
    }
    std::string x_0 = HexStringFromLibsnarkBigint(aff.X.c0.as_bigint());
    while (x_0.size() < 64)
    {
        x_0 = "0" + x_0;
    }
    std::string y_1 = HexStringFromLibsnarkBigint(aff.Y.c1.as_bigint());
    while (y_1.size() < 64)
    {
        y_1 = "0" + y_1;
    }
    std::string y_0 = HexStringFromLibsnarkBigint(aff.Y.c0.as_bigint());
    while (y_0.size() < 64)
    {
        y_0 = "0" + y_0;
    }
    return x_1 + x_0 + y_1 + y_0;
}
std::string string_proof_as_hex(libsnark::r1cs_gg_ppzksnark_proof<libff::alt_bn128_pp> proof)
{
    std::string A = outputPointG1AffineAsHex(proof.g_A);

    std::string B = outputPointG2AffineAsHex(proof.g_B);

    std::string C = outputPointG1AffineAsHex(proof.g_C);

    std::string proof_string = A + B + C;

    return proof_string;
}

template <typename ppzksnark_ppT>
r1cs_gg_ppzksnark_proof<ppzksnark_ppT> generate_transfer_proof(r1cs_gg_ppzksnark_proving_key<ppzksnark_ppT> proving_key,
                                                           const Note &note_old,
                                                           const Note &note,
                                                           uint64_t v_s,
                                                           uint256 r_s_data,
                                                           uint256 sk_data,
                                                           uint256 cmtS,
                                                           uint256 cmtA_old,
                                                           uint256 cmtA,
                                                           const uint256 &rt,
                                                           const MerklePath &path,
                                                           uint8_t type_val
                                                           )
{
    typedef Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;
    transfer_gadget<FieldT> transfer(pb);
    transfer.generate_r1cs_constraints();

    transfer.generate_r1cs_witness(note_old, note, v_s, r_s_data, sk_data, cmtS, cmtA_old, cmtA, rt, path, type_val);

    if (!pb.is_satisfied())
    {
        cout << "can not generate transfer proof" << endl;
        return r1cs_gg_ppzksnark_proof<ppzksnark_ppT>();
    }

    return r1cs_gg_ppzksnark_prover<ppzksnark_ppT>(proving_key, pb.primary_input(), pb.auxiliary_input());
}

template <typename ppzksnark_ppT>
bool verify_transfer_proof(r1cs_gg_ppzksnark_verification_key<ppzksnark_ppT> verification_key,
                          r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof,
                          const uint256 &cmtS,
                          const uint256 &sn_old,
                          const uint256 &cmtA,
                          const uint256 &rt,
                          bool type
                          )
{
    typedef Fr<ppzksnark_ppT> FieldT;

    const r1cs_primary_input<FieldT> input = transfer_gadget<FieldT>::witness_map(
        cmtS,
        sn_old,
        cmtA,
        rt,
        type
    );

    return r1cs_gg_ppzksnark_verifier_strong_IC<ppzksnark_ppT>(verification_key, input, proof);
}

char *genCMT(uint64_t value, char *sn_string, char *r_string)
{
    uint256 sn = uint256S(sn_string);
    uint256 r = uint256S(r_string);
    Note note = Note(value, sn, r);
    uint256 cmtA = note.cm();
    std::string cmtA_c = cmtA.ToString();

    char *p = new char[67];
    cmtA_c.copy(p, 66, 0);
    *(p + 66) = '\0';

    return p;
}

char *genCMTStransfer(uint64_t value_s, char *r_s_string)
{
    uint256 r_s = uint256S(r_s_string);
    NoteS notes = NoteS(value_s, r_s);
    uint256 cmtS = notes.cm();

    std::string cmtS_c = cmtS.ToString();

    char *p = new char[67];
    cmtS_c.copy(p, 66, 0);
    *(p + 66) = '\0';

    return p;
}

char* computePRF(char* sk_string, char* r_string)
{
    uint256 sk = uint256S(sk_string);
    uint256 r = uint256S(r_string);
    uint256 sn = Compute_PRF(sk, r);
    std::string sn_c = sn.ToString();

    char *p = new char[65];
    sn_c.copy(p, 64, 0);
    *(p + 64) = '\0';

    return p;
}

char *genRoot(char *cmtarray, int n)
{
    boost::array<uint256, 256> commitments;

    string s = cmtarray;

    ZCIncrementalMerkleTree tree;
    assert(tree.root() == ZCIncrementalMerkleTree::empty_root());

    for (int i = 0; i < n; i++)
    {
        commitments[i] = uint256S(s.substr(i * 66, 66));
        tree.append(commitments[i]);
    }

    uint256 rt = tree.root();
    std::string rt_c = rt.ToString();

    char *p = new char[65];
    rt_c.copy(p, 64, 0);
    *(p + 64) = '\0';

    return p;
}

char *genTransferproof(uint64_t value,
                      uint64_t value_old,
                      char *sn_old_string,
                      char *r_old_string,
                      char *sn_string,
                      char *r_new_string,
                      char *cmtA_old_string,
                      char *cmtA_string,
                      uint64_t value_s,
                      char *sk_string,
                      char *r_s_string,
                      char *cmtarray,
                      int n,
                      char *RT,
                      uint8_t type_val
                      )
{
    uint256 sn_old = uint256S(sn_old_string);
    uint256 r_old = uint256S(r_old_string);
    uint256 sn = uint256S(sn_string);
    uint256 r_new = uint256S(r_new_string);
    uint256 cmtA_old = uint256S(cmtA_old_string);
    uint256 cmtA = uint256S(cmtA_string);
    uint256 sk = uint256S(sk_string);
    uint256 r_s = uint256S(r_s_string);

    Note note_old = Note(value_old, sn_old, r_old);

    Note note = Note(value, sn, r_new);

    NoteS note_s = NoteS(value_s, r_s);
    uint256 cmtS = note_s.cm();

    boost::array<uint256, 256> commitments;
    string sss = cmtarray;

    for (int i = 0; i < n; i++)
    {
        commitments[i] = uint256S(sss.substr(i * 66, 66));
    }

    MerklePath path;
    uint256 rt;
    try {
        ZCIncrementalMerkleTree tree;
        assert(tree.root() == ZCIncrementalMerkleTree::empty_root());

        ZCIncrementalWitness wit = tree.witness();
        bool find_cmtA_old = false;
        for (size_t i = 0; i < n; i++)
        {
            if (find_cmtA_old)
            {
                wit.append(commitments[i]);
            }
            else
            {
                tree.append(commitments[i]);
            }

            if (commitments[i] == cmtA_old)
            {
                wit = tree.witness();
                find_cmtA_old = true;
            }
        }
        if (!find_cmtA_old) {
            printf("transfer: cmtA_old not found in commitments\n");
            fflush(stdout);
            char *p = new char[1153];
            memset(p, '0', 1152);
            *(p + 1152) = '\0';
            return p;
        }
        path = wit.path();
        rt = wit.root();
    } catch (const std::exception &e) {
        printf("transfer: merkle tree error: %s\n", e.what());
        fflush(stdout);
        char *p = new char[1153];
        memset(p, '0', 1152);
        *(p + 1152) = '\0';
        return p;
    }

alt_bn128_pp::init_public_params();

    static r1cs_gg_ppzksnark_proving_key<alt_bn128_pp> cached_pk;
    static bool pk_loaded = false;
    if (!pk_loaded) {
        cached_pk = deserializeProvingKeyFromFile("/usr/local/prfKey/transferpk.txt");
        pk_loaded = true;
    }
r1cs_gg_ppzksnark_keypair<alt_bn128_pp> keypair;
    keypair.pk = cached_pk;

    struct timeval proof_start, proof_end;
    double proof_timeuse;
    gettimeofday(&proof_start, NULL);

    libsnark::r1cs_gg_ppzksnark_proof<libff::alt_bn128_pp> proof = generate_transfer_proof<alt_bn128_pp>(keypair.pk,
                                                                                                       note_old,
                                                                                                       note,
                                                                                                       value_s,
                                                                                                       r_s,
                                                                                                       sk,
                                                                                                       cmtS,
                                                                                                       cmtA_old,
                                                                                                       cmtA,
                                                                                                       rt,
                                                                                                       path,
                                                                                                       type_val);

    gettimeofday(&proof_end, NULL);
    proof_timeuse = proof_end.tv_sec - proof_start.tv_sec + (proof_end.tv_usec - proof_start.tv_usec)/1000000.0;
    printf("\n\n gen transfer proof Use Time:%fs\n\n", proof_timeuse);
    fflush(stdout);

    std::string proof_string = string_proof_as_hex(proof);

    char *p = new char[1153];
    proof_string.copy(p, 1152, 0);
    *(p + 1152) = '\0';

    return p;
}

bool verifyTransferproof(char *data, char *cmtS_string, char *sn_old_string, char *cmtA_string, char *rt_string, uint64_t value_s, uint8_t type_val)
{
    uint256 cmtS = uint256S(cmtS_string);
    uint256 sn_old = uint256S(sn_old_string);
    uint256 cmtA = uint256S(cmtA_string);
    uint256 rt = uint256S(rt_string);
    bool type = type_val ? true : false;

    alt_bn128_pp::init_public_params();

    static r1cs_gg_ppzksnark_verification_key<alt_bn128_pp> cached_vk;
    static bool vk_loaded = false;
    if (!vk_loaded) {
        cached_vk = deserializevkFromFile("/usr/local/prfKey/transfervk.txt");
        vk_loaded = true;
    }
    r1cs_gg_ppzksnark_keypair<alt_bn128_pp> keypair;
    keypair.vk = cached_vk;

    libsnark::r1cs_gg_ppzksnark_proof<libff::alt_bn128_pp> proof;

    uint8_t A_x[64];
    uint8_t A_y[64];

    uint8_t B_x_1[64];
    uint8_t B_x_0[64];
    uint8_t B_y_1[64];
    uint8_t B_y_0[64];

    uint8_t C_x[64];
    uint8_t C_y[64];

    for (int i = 0; i < 64; i++)
    {
        A_x[i] = uint8_t(data[i + 0]);
        A_y[i] = uint8_t(data[i + 64]);

        B_x_1[i] = uint8_t(data[i + 128]);
        B_x_0[i] = uint8_t(data[i + 192]);
        B_y_1[i] = uint8_t(data[i + 256]);
        B_y_0[i] = uint8_t(data[i + 320]);

        C_x[i] = uint8_t(data[i + 384]);
        C_y[i] = uint8_t(data[i + 448]);
    }

    for (int i = 0, j = 0; i < 64; i += 2, j++)
    {
        A_x[j] = uint8_t(convertFromAscii(A_x[i]) * 16 + convertFromAscii(A_x[i + 1]));
        A_y[j] = uint8_t(convertFromAscii(A_y[i]) * 16 + convertFromAscii(A_y[i + 1]));

        B_x_1[j] = uint8_t(convertFromAscii(B_x_1[i]) * 16 + convertFromAscii(B_x_1[i + 1]));
        B_x_0[j] = uint8_t(convertFromAscii(B_x_0[i]) * 16 + convertFromAscii(B_x_0[i + 1]));
        B_y_1[j] = uint8_t(convertFromAscii(B_y_1[i]) * 16 + convertFromAscii(B_y_1[i + 1]));
        B_y_0[j] = uint8_t(convertFromAscii(B_y_0[i]) * 16 + convertFromAscii(B_y_0[i + 1]));

        C_x[j] = uint8_t(convertFromAscii(C_x[i]) * 16 + convertFromAscii(C_x[i + 1]));
        C_y[j] = uint8_t(convertFromAscii(C_y[i]) * 16 + convertFromAscii(C_y[i + 1]));
    }

    libff::bigint<libff::alt_bn128_r_limbs> a_x = libsnarkBigintFromBytes(A_x);
    libff::bigint<libff::alt_bn128_r_limbs> a_y = libsnarkBigintFromBytes(A_y);

    libff::bigint<libff::alt_bn128_r_limbs> b_x_1 = libsnarkBigintFromBytes(B_x_1);
    libff::bigint<libff::alt_bn128_r_limbs> b_x_0 = libsnarkBigintFromBytes(B_x_0);
    libff::bigint<libff::alt_bn128_r_limbs> b_y_1 = libsnarkBigintFromBytes(B_y_1);
    libff::bigint<libff::alt_bn128_r_limbs> b_y_0 = libsnarkBigintFromBytes(B_y_0);

    libff::bigint<libff::alt_bn128_r_limbs> c_x = libsnarkBigintFromBytes(C_x);
    libff::bigint<libff::alt_bn128_r_limbs> c_y = libsnarkBigintFromBytes(C_y);

    proof.g_A.X = a_x;
    proof.g_A.Y = a_y;

    proof.g_B.X.c1 = b_x_1;
    proof.g_B.X.c0 = b_x_0;
    proof.g_B.Y.c1 = b_y_1;
    proof.g_B.Y.c0 = b_y_0;

proof.g_C.X = c_x;
    proof.g_C.Y = c_y;

    struct timeval verify_start, verify_end;
    double verify_timeuse;
    gettimeofday(&verify_start, NULL);

    bool result = verify_transfer_proof(keypair.vk,
                                        proof,
                                        cmtS,
                                        sn_old,
                                        cmtA,
                                        rt,
                                        type);

    gettimeofday(&verify_end, NULL);
    verify_timeuse = verify_end.tv_sec - verify_start.tv_sec + (verify_end.tv_usec - verify_start.tv_usec)/1000000.0;
    printf("\n\n verify transfer proof Use Time:%fs\n\n", verify_timeuse);
    fflush(stdout);

    if (!result)
    {
        cout << "Verifying transfer proof unsuccessfully!!!" << endl;
    }
    else
    {
        // cout << "Verifying transfer proof successfully!!!" << endl;
    }

    return result;
}
