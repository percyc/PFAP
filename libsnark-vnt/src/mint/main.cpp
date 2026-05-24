#include <stdio.h>
#include <iostream>

#include <boost/optional.hpp>
#include <boost/foreach.hpp>
#include <boost/format.hpp>

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

// 生成proof
template<typename ppzksnark_ppT>
boost::optional<r1cs_gg_ppzksnark_proof<ppzksnark_ppT>> generate_mint_proof(r1cs_gg_ppzksnark_proving_key<ppzksnark_ppT> proving_key,
                                                                    const Note& note_old,
                                                                    const Note& note,
                                                                    uint256 cmtA_old,
                                                                    uint256 cmtA,
                                                                    uint64_t value_s,
                                                                    uint256 sk_data,
                                                                    const uint256& rt,
                                                                    const MerklePath& path
                                                                   )
{
    typedef Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;  // 定义原始模型，该模型包含constraint_system成员变量
    mint_gadget<FieldT> mint(pb); // 构造新模型
    mint.generate_r1cs_constraints(); // 生成约束

    mint.generate_r1cs_witness(note_old, note, cmtA_old, cmtA, value_s, sk_data, rt, path); // 为新模型的参数生成证明

    cout << "pb.is_satisfied() is " << pb.is_satisfied() << endl;

    if (!pb.is_satisfied()) { // 三元组R1CS是否满足  < A , X > * < B , X > = < C , X >
        return boost::none;
    }

    // 调用libsnark库中生成proof的函数
    return r1cs_gg_ppzksnark_prover<ppzksnark_ppT>(proving_key, pb.primary_input(), pb.auxiliary_input());
}

// 验证proof
template<typename ppzksnark_ppT>
bool verify_mint_proof(r1cs_gg_ppzksnark_verification_key<ppzksnark_ppT> verification_key,
                    r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof,
                    const uint256& sn_old,
                    const uint256& rt,
                    const uint256& cmtA,
                    uint64_t value_s
                  )
{
    typedef Fr<ppzksnark_ppT> FieldT;

    const r1cs_primary_input<FieldT> input = mint_gadget<FieldT>::witness_map(
        sn_old,
        rt,
        cmtA,
        value_s
    ); 

    // 调用libsnark库中验证proof的函数
    return r1cs_gg_ppzksnark_verifier_strong_IC<ppzksnark_ppT>(verification_key, input, proof);
}

template<typename ppzksnark_ppT>
void PrintProof(r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof)
{
    printf("================== Print proof ==================================\n");
    //printf("proof is %x\n", *proof);
    std::cout << "mint proof:\n";

    std::cout << "\n knowledge_commitment<G1<ppT>, G1<ppT> > g_A: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_A.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_A.h << endl;

    std::cout << "\n knowledge_commitment<G2<ppT>, G1<ppT> > g_B: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_B.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_B.h << endl;

    std::cout << "\n knowledge_commitment<G1<ppT>, G1<ppT> > g_C: ";
    std::cout << "\n   knowledge_commitment.g: \n     " << proof.g_C.g;
    std::cout << "\n   knowledge_commitment.h: \n     " << proof.g_C.h << endl;


    std::cout << "\n G1<ppT> g_H: " << proof.g_H << endl;
    std::cout << "\n G1<ppT> g_K: " << proof.g_K << endl;
    printf("=================================================================\n");
}

template<typename ppzksnark_ppT> //--Agzs
bool test_mint_gadget_with_instance(
                            uint64_t value,
                            uint64_t value_old,
                            //uint256 sn_old,
                            //uint256 r_old,
                            //uint256 sn,
                            //uint256 r,
                            //uint256 cmtA_old,
                            //uint256 cmtA,
                            uint64_t value_s,
                            r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> keypair
                        )
{
    // Note note_old = Note(value_old, sn_old, r_old);
    // Note note = Note(value, sn, r);

    // uint256 sn_test = random_uint256();
    // uint256 r_test = random_uint256();

    uint256 sk = uint256S("1");//random_uint256();
    uint256 wrong_sk = uint256S("2");//random_uint256();
    
    printf("Testing with sk=%s\n", sk.ToString().c_str());

    uint256 r_old = uint256S("123456");//random_uint256();   
    uint256 sn_old = Compute_PRF(sk, r_old);//random_uint256();
    Note note_old = Note(value_old, sn_old, r_old);
    uint256 cmtA_old = note_old.cm();

    uint256 r = uint256S("123");//random_uint256();
    uint256 sn = Compute_PRF(sk, sn_old);//random_uint256();
    Note note = Note(value, sn, r);
    uint256 cmtA = note.cm();

    // Create merkle tree - use same pattern as deposit
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
            //在要证明的叶子节点添加到tree后，才算真正初始化wit，下面的root和path才会正确。
            wit = tree.witness();
            find_cmtA_old = true;
        }
    }
    
    auto path = wit.path();
    uint256 rt = wit.root();
    
    cout << "tree.root = 0x" << tree.root().ToString() << endl;
    cout << "wit.root = 0x" << wit.root().ToString() << endl;

    // 生成proof
    cout << "Trying to generate proof..." << endl;

    struct timeval gen_start, gen_end;
    double mintTimeUse;
    gettimeofday(&gen_start,NULL);

    auto proof = generate_mint_proof<default_r1cs_gg_ppzksnark_pp>(keypair.pk, 
                                                            note_old,
                                                            note,
                                                            cmtA_old,
                                                            cmtA,
                                                            value_s,
                                                            sk,
                                                            rt,
                                                            path
                                                            );

    gettimeofday(&gen_end, NULL);
    mintTimeUse = gen_end.tv_sec - gen_start.tv_sec + (gen_end.tv_usec - gen_start.tv_usec)/1000000.0;
    printf("\n\nGen Mint Proof Use Time:%fs\n\n", mintTimeUse);

    // verify proof
    if (!proof) {
        printf("generate mint proof fail!!!\n");
        return false;
    } else {
        //PrintProof(*proof);

        //assert(verify_mint_proof(keypair.vk, *proof));
        // wrong test data
        uint256 wrong_sn_old = uint256S("666");//random_uint256();
        uint64_t wrong_value_s = uint64_t(100);
        uint256 wrong_cmtA_old = note.cm();
        uint256 wrong_cmtA = note_old.cm();        

        struct timeval ver_start, ver_end;
        double mintVerTimeUse;
        gettimeofday(&ver_start, NULL);

        bool result = verify_mint_proof(keypair.vk, 
                                   *proof, 
                                   sn_old,
                                   rt,
                                   cmtA,
                                   value_s
                                   );

        gettimeofday(&ver_end, NULL);
        mintVerTimeUse = ver_end.tv_sec - ver_start.tv_sec + (ver_end.tv_usec - ver_start.tv_usec)/1000000.0;
        printf("\n\nVer Mint Proof Use Time:%fs\n\n", mintVerTimeUse);
        //printf("verify result = %d\n", result);
         
        if (!result){
            cout << "Verifying mint proof unsuccessfully!!!" << endl;
        } else {
            cout << "Verifying mint proof successfully!!!" << endl;
        }
        
        return result;
    }
}

template<typename ppzksnark_ppT>
r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> Setup() {
    default_r1cs_gg_ppzksnark_pp::init_public_params();
    
    typedef libff::Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;

    mint_gadget<FieldT> mint(pb);
    mint.generate_r1cs_constraints();// 生成约束

    // check conatraints
    const r1cs_constraint_system<FieldT> constraint_system = pb.get_constraint_system();
    std::cout << "Number of R1CS constraints: " << constraint_system.num_constraints() << endl;
    
    // key pair generation
    r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> keypair = r1cs_gg_ppzksnark_generator<ppzksnark_ppT>(constraint_system);

    return keypair;
}

int main () {
    struct timeval t1, t2;
    double timeuse;
    gettimeofday(&t1,NULL);

    //default_r1cs_gg_ppzksnark_pp::init_public_params();
    r1cs_gg_ppzksnark_keypair<default_r1cs_gg_ppzksnark_pp> keypair = Setup<default_r1cs_gg_ppzksnark_pp>();

    gettimeofday(&t2,NULL);
    timeuse = t2.tv_sec - t1.tv_sec + (t2.tv_usec - t1.tv_usec)/1000000.0;
    printf("\n\nMint Setup Time Usage:%fs\n\n",timeuse);
    //test_r1cs_gg_ppzksnark<dsefault_r1cs_gg_ppzksnark_pp>(1000, 100);
   
    //r1cs_gg_ppzksnark_keypair<default_r1cs_gg_ppzksnark_pp> keypair = Setup<default_r1cs_gg_ppzksnark_pp>();

    libff::print_header("#             testing mint gadget");

    uint64_t value = uint64_t(13); 
    uint64_t value_old = uint64_t(6); 
    uint64_t value_s = uint64_t(7);

    bool result1 = test_mint_gadget_with_instance<default_r1cs_gg_ppzksnark_pp>(value, value_old, value_s, keypair);
    printf("test_mint (value=13,old=6,s=7): %s\n\n", result1 ? "PASS" : "FAIL");

    // ORIGINAL test, but single-element tree
    printf("\n--- ORIGINAL test (value_old=6), single-element tree ---\n");
    {
        uint256 t_sk = uint256S("1");
        uint256 t_r_old = uint256S("123456");
        uint256 t_sn_old = Compute_PRF(t_sk, t_r_old);
        Note t_note_old = Note(6, t_sn_old, t_r_old);
        uint256 t_cmtA_old = t_note_old.cm();

        uint256 t_r = uint256S("123");
        uint256 t_sn = Compute_PRF(t_sk, t_sn_old);
        Note t_note = Note(13, t_sn, t_r);
        uint256 t_cmtA = t_note.cm();

        ZCIncrementalMerkleTree tree;
        tree.append(t_cmtA_old);
        ZCIncrementalWitness wit = tree.witness();
        auto path = wit.path();
        uint256 rt = tree.root();

        typedef Fr<default_r1cs_gg_ppzksnark_pp> FieldT;
        protoboard<FieldT> pb;
        mint_gadget<FieldT> mint(pb);
        mint.generate_r1cs_constraints();
        mint.generate_r1cs_witness(t_note_old, t_note, t_cmtA_old, t_cmtA, 7, t_sk, rt, path);
        printf("value_old=6, single-tree test is_satisfied=%d\n", pb.is_satisfied());
    }

    // Test with Go-simulated params but non-zero r_old
    printf("\n--- Go-param test with non-zero r_old ---\n");
    uint256 sk = uint256S("000000000000000000000000ffffffffffffffffffffffffffffffffffffffff");
    uint64_t test_value_s = uint64_t(1);
    uint256 r_old2 = uint256S("0000000000000000000000000000000000000000000000000000000000000001");
    uint256 sn_old2 = Compute_PRF(sk, r_old2);
    Note note_old2 = Note(uint64_t(0), sn_old2, r_old2);
    uint256 cmtA_old2 = note_old2.cm();

    uint256 r_new2 = uint256S("77dd3f66126cd98ae447525c5418f3ebd40b3989a72162e83fd466fd17ae8aea");
    uint256 sn_new2 = Compute_PRF(sk, sn_old2);
    Note note2 = Note(uint64_t(1), sn_new2, r_new2);
    uint256 cmtA_new2 = note2.cm();

    {
        ZCIncrementalMerkleTree tree2;
        tree2.append(cmtA_old2);
        ZCIncrementalWitness wit2 = tree2.witness();
        auto path2 = wit2.path();
        uint256 rt2 = tree2.root();

        protoboard<Fr<default_r1cs_gg_ppzksnark_pp>> pb2;
        mint_gadget<Fr<default_r1cs_gg_ppzksnark_pp>> mint2(pb2);
        mint2.generate_r1cs_constraints();
        mint2.generate_r1cs_witness(note_old2, note2, cmtA_old2, cmtA_new2, test_value_s, sk, rt2, path2);
        printf("Non-zero r_old test is_satisfied=%d\n", pb2.is_satisfied());
    }

    // Test with orig sk and multi-element tree (like original test)
    printf("\n--- Test with sk=1, multi-element tree (100) ---\n");
    {
        uint256 orig_sk = uint256S("1");
        uint256 orig_r_old = uint256S("123456");
        uint256 orig_sn_old = Compute_PRF(orig_sk, orig_r_old);
        Note orig_note_old = Note(uint64_t(0), orig_sn_old, orig_r_old);
        uint256 orig_cmtA_old = orig_note_old.cm();
        
        uint256 orig_r = uint256S("123");
        uint256 orig_sn = Compute_PRF(orig_sk, orig_sn_old);
        Note orig_note = Note(uint64_t(1), orig_sn, orig_r);
        uint256 orig_cmtA = orig_note.cm();
        
        // Multi-element tree like original test
        boost::array<uint256, 256> commitments;
        ZCIncrementalMerkleTree otree2;
        ZCIncrementalWitness owit = otree2.witness();
        bool found = false;
        for (size_t i = 0; i < 100; i++) {
            if (i < 10) commitments[i] = uint256S("111111");
            else if (i == 10) commitments[i] = orig_cmtA_old;
            else commitments[i] = uint256S("222222");
            
            if (found) owit.append(commitments[i]);
            else otree2.append(commitments[i]);
            
            if (commitments[i] == orig_cmtA_old) {
                owit = otree2.witness();
                found = true;
            }
        }
        auto opath2 = owit.path();
        uint256 ort2 = owit.root();
        
        protoboard<Fr<default_r1cs_gg_ppzksnark_pp>> opb2;
        mint_gadget<Fr<default_r1cs_gg_ppzksnark_pp>> omint2(opb2);
        omint2.generate_r1cs_constraints();
        omint2.generate_r1cs_witness(orig_note_old, orig_note, orig_cmtA_old, orig_cmtA, uint64_t(1), orig_sk, ort2, opath2);
        printf("multi-element tree test is_satisfied=%d\n", opb2.is_satisfied());
    }
    
    // Original Go-param test with zero r_old
    printf("\n--- Go-param test with zero r_old ---\n");
    uint256 r_old = uint256S("0000000000000000000000000000000000000000000000000000000000000000");
    uint256 sn_old = Compute_PRF(sk, r_old);
    Note note_old = Note(uint64_t(0), sn_old, r_old);
    uint256 cmtA_old = note_old.cm();

    uint256 r_new = uint256S("77dd3f66126cd98ae447525c5418f3ebd40b3989a72162e83fd466fd17ae8aea");
    uint256 sn_new = Compute_PRF(sk, sn_old);
    Note note = Note(uint64_t(1), sn_new, r_new);
    uint256 cmtA_new = note.cm();
    uint64_t value_s_zero = uint64_t(1);

    ZCIncrementalMerkleTree tree;
    tree.append(cmtA_old);
    ZCIncrementalWitness wit = tree.witness();
    auto path = wit.path();
    uint256 rt = tree.root();

protoboard<Fr<default_r1cs_gg_ppzksnark_pp>> pb;
    mint_gadget<Fr<default_r1cs_gg_ppzksnark_pp>> mint(pb);
    mint.generate_r1cs_constraints();
    mint.generate_r1cs_witness(note_old, note, cmtA_old, cmtA_new, value_s_zero, sk, rt, path);

    // Check circuit variables match expected values
    printf("Circuit sn_old vs note_old.sn: %d\n", 
        sn_old.ToString() == note_old.sn.ToString());
    printf("Circuit sn vs note.sn: %d\n", 
        sn_new.ToString() == note.sn.ToString());
    
    // Verify commitment check
    printf("Circuit cmtA_old vs note_old.cm(): %d\n", 
        cmtA_old.ToString() == note_old.cm().ToString());
    printf("Circuit cmtA_new vs note.cm(): %d\n", 
        cmtA_new.ToString() == note.cm().ToString());

    printf("Go-param test is_satisfied=%d\n", pb.is_satisfied());

    // Note. cmake can not compile the assert()  --Agzs
    
    return 0;
}

