#include <stdio.h>
#include <iostream>

#include <boost/optional.hpp>
#include <boost/foreach.hpp>
#include <boost/format.hpp>
#include <boost/array.hpp>

#include "libsnark/zk_proof_systems/ppzksnark/r1cs_gg_ppzksnark/r1cs_gg_ppzksnark.hpp"
#include "libsnark/common/default_types/r1cs_gg_ppzksnark_pp.hpp"
#include <libsnark/gadgetlib1/gadgets/hashes/sha256/sha256_gadget.hpp>
#include "libsnark/gadgetlib1/gadgets/merkle_tree/merkle_tree_check_read_gadget.hpp"

#include<sys/time.h>

#include "Note.h"
#include "IncrementalMerkleTree.hpp"

using namespace libsnark;
using namespace libff;
using namespace std;
using namespace libvnt;

#include "circuit/gadget.tcc"

#define DEBUG 0

template<typename ppzksnark_ppT>
boost::optional<r1cs_gg_ppzksnark_proof<ppzksnark_ppT>> generate_transfer_proof(
    r1cs_gg_ppzksnark_proving_key<ppzksnark_ppT> proving_key,
    const Note& note_old,
    const Note& note,
    uint64_t v_s,
    uint256 r_s_data,
    uint256 sk_data,
    uint256 cmtS_data,
    uint256 cmtA_old_data,
    uint256 cmtA_data,
    const uint256& rt,
    const MerklePath& path,
    uint8_t type_val
)
{
    typedef Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;
    transfer_gadget<FieldT> g(pb);
    g.generate_r1cs_constraints();

    g.generate_r1cs_witness(note_old, note, v_s, r_s_data, sk_data, cmtS_data, cmtA_old_data, cmtA_data, rt, path, type_val);

    cout << "pb.is_satisfied() is " << pb.is_satisfied() << endl;

    if (!pb.is_satisfied()) {
        return boost::none;
    }

    return r1cs_gg_ppzksnark_prover<ppzksnark_ppT>(proving_key, pb.primary_input(), pb.auxiliary_input());
}

template<typename ppzksnark_ppT>
bool verify_transfer_proof(
    r1cs_gg_ppzksnark_verification_key<ppzksnark_ppT> verification_key,
    r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof,
    const uint256& cmtS,
    const uint256& sn_old,
    const uint256& cmtA,
    const uint256& rt,
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

template<typename ppzksnark_ppT>
void PrintProof(r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof)
{
    printf("================== Print proof ==================================\n");
    std::cout << "transfer proof:\n";

    std::cout << "\n knowledge_commitment<G1<ppT>, G1<ppT>> g_A: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_A.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_A.h << endl;

    std::cout << "\n knowledge_commitment<G2<ppT>, G1<ppT>> g_B: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_B.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_B.h << endl;

    std::cout << "\n knowledge_commitment<G1<ppT>, G1<ppT>> g_C: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_C.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_C.h << endl;

    std::cout << "\n G1<ppT> g_H: " << proof.g_H << endl;
    std::cout << "\n G1<ppT> g_K: " << proof.g_K << endl;
    printf("=================================================================\n");
}

template<typename ppzksnark_ppT>
bool test_transfer_gadget_with_instance(
    uint64_t value,
    uint64_t value_old,
    uint64_t value_s,
    r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> keypair,
    uint8_t type_val
)
{
    uint256 sk = uint256S("1");

    uint256 r_old = uint256S("123456");
    uint256 sn_old = Compute_PRF(sk, r_old);
    Note note_old = Note(value_old, sn_old, r_old);
    uint256 cmtA_old = note_old.cm();

    uint256 r_new = uint256S("123");
    uint256 sn = Compute_PRF(sk, sn_old);
    Note note = Note(value, sn, r_new);
    uint256 cmtA = note.cm();

    uint256 r_s = uint256S("9999");
    NoteS note_s = NoteS(value_s, r_s);
    uint256 cmtS = note_s.cm();

    boost::array<uint256, 256> commitments;
    ZCIncrementalMerkleTree tree;
    assert(tree.root() == ZCIncrementalMerkleTree::empty_root());

    ZCIncrementalWitness wit = tree.witness();
    bool find_cmtA_old = false;
    for (size_t i = 0; i < 100; i++) {
        if (i < 10) {
            commitments[i] = uint256S("111111");
        } else if (i == 10) {
            commitments[i] = cmtA_old;
        } else {
            commitments[i] = uint256S("222222");
        }
        
        if (find_cmtA_old) {
            wit.append(commitments[i]);
        } else {
            tree.append(commitments[i]);
        }
        
        if (commitments[i] == cmtA_old) {
            // The witness is only truly initialized after the leaf to be proved has been appended to the tree; only then are the root and path below correct.
            wit = tree.witness();
            find_cmtA_old = true;
        }
    }

    auto path = wit.path();
    uint256 rt = wit.root();
    
    cout << "tree.root = 0x" << tree.root().ToString() << endl;
    cout << "wit.root = 0x" << wit.root().ToString() << endl;

    cout << "Trying to generate transfer proof..." << endl;

    struct timeval gen_start, gen_end;
    struct timeval ver_start, ver_end;
    double genTimeUse;
    double verTimeUse;
    gettimeofday(&gen_start,NULL);

    auto proof = generate_transfer_proof<default_r1cs_gg_ppzksnark_pp>(keypair.pk,
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
                                                                    type_val
                                                                    );

    gettimeofday(&gen_end, NULL);
    genTimeUse = gen_end.tv_sec - gen_start.tv_sec + (gen_end.tv_usec - gen_start.tv_usec)/1000000.0;
    printf("\n\nGen Transfer Proof Use Time:%fs\n\n", genTimeUse);

    if (!proof) {
        printf("generate transfer proof fail!!!\n");
        return false;
    } else {
        gettimeofday(&ver_start,NULL);
        bool result = verify_transfer_proof(keypair.vk,
                                           *proof,
                                           cmtS,
                                           sn_old,
                                           cmtA,
                                           rt,
                                           type_val != 0
                                           );

        if (!result){
            cout << "Verifying transfer proof unsuccessfully!!!" << endl;
        } else {
            gettimeofday(&ver_end,NULL);
            cout << "Verifying transfer proof successfully!!!" << endl;
            verTimeUse = ver_end.tv_sec - ver_start.tv_sec + (ver_end.tv_usec - ver_start.tv_usec)/1000000.0;
            printf("\n\nVerifying Transfer Proof Use Time:%fs\n\n", verTimeUse);
        }

        return result;
    }
}

template<typename ppzksnark_ppT>
r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> Setup() {
    default_r1cs_gg_ppzksnark_pp::init_public_params();

    typedef libff::Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;

    transfer_gadget<FieldT> transfer(pb);
    transfer.generate_r1cs_constraints();

    const r1cs_constraint_system<FieldT> constraint_system = pb.get_constraint_system();
    std::cout << "Number of R1CS constraints: " << constraint_system.num_constraints() << endl;

    r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> keypair = r1cs_gg_ppzksnark_generator<ppzksnark_ppT>(constraint_system);

    return keypair;
}

int main () {
    struct timeval t1, t2;
    double timeuse;
    gettimeofday(&t1,NULL);

    r1cs_gg_ppzksnark_keypair<default_r1cs_gg_ppzksnark_pp> keypair = Setup<default_r1cs_gg_ppzksnark_pp>();

    gettimeofday(&t2,NULL);
    timeuse = t2.tv_sec - t1.tv_sec + (t2.tv_usec - t1.tv_usec)/1000000.0;
    printf("\n\nTransfer Setup Time Usage:%fs\n\n",timeuse);

    libff::print_header("#             testing transfer gadget - payer case (type=0, subtract)");

    uint64_t value = uint64_t(6);
    uint64_t value_old = uint64_t(13);
    uint64_t value_s = uint64_t(7);

    test_transfer_gadget_with_instance<default_r1cs_gg_ppzksnark_pp>(value, value_old, value_s, keypair, 0);

    libff::print_header("#             testing transfer gadget - payee case (type=1, add)");

    value = uint64_t(20);
    value_old = uint64_t(13);
    value_s = uint64_t(7);

    test_transfer_gadget_with_instance<default_r1cs_gg_ppzksnark_pp>(value, value_old, value_s, keypair, 1);

    return 0;
}
