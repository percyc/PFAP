#include <stdio.h>
#include <iostream>

#include <boost/optional.hpp>
#include <boost/foreach.hpp>
#include <boost/format.hpp>

#include "libsnark/zk_proof_systems/ppzksnark/r1cs_gg_ppzksnark/r1cs_gg_ppzksnark.hpp"
#include "libsnark/common/default_types/r1cs_gg_ppzksnark_pp.hpp"
#include <libsnark/gadgetlib1/gadgets/hashes/sha256/sha256_gadget.hpp>

#include<sys/time.h>

#include "Note.h"

using namespace libsnark;
using namespace libff;
using namespace std;

#include "circuit/gadget.tcc"

#define DEBUG 0

template<typename ppzksnark_ppT>
boost::optional<r1cs_gg_ppzksnark_proof<ppzksnark_ppT>> generate_createAccount_proof(
    r1cs_gg_ppzksnark_proving_key<ppzksnark_ppT> proving_key,
    uint256 sk_data,
    uint256 r_A_data,
    uint256 sn_A_data,
    uint256 cmtA_data
)
{
    typedef Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;
    createAccount_gadget<FieldT> g(pb);
    g.generate_r1cs_constraints();

    g.generate_r1cs_witness(sk_data, r_A_data, sn_A_data, cmtA_data);

    cout << "pb.is_satisfied() is " << pb.is_satisfied() << endl;

    if (!pb.is_satisfied()) {
        return boost::none;
    }

    return r1cs_gg_ppzksnark_prover<ppzksnark_ppT>(proving_key, pb.primary_input(), pb.auxiliary_input());
}

template<typename ppzksnark_ppT>
bool verify_createAccount_proof(
    r1cs_gg_ppzksnark_verification_key<ppzksnark_ppT> verification_key,
    r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof,
    const uint256& cmtA
)
{
    typedef Fr<ppzksnark_ppT> FieldT;

    const r1cs_primary_input<FieldT> input = createAccount_gadget<FieldT>::witness_map(cmtA);

    return r1cs_gg_ppzksnark_verifier_strong_IC<ppzksnark_ppT>(verification_key, input, proof);
}

template<typename ppzksnark_ppT>
void PrintProof(r1cs_gg_ppzksnark_proof<ppzksnark_ppT> proof)
{
    printf("================== Print proof ==================================\n");
    std::cout << "createAccount proof:\n";

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
bool test_createAccount_gadget_with_instance(
    r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> keypair
)
{
    uint256 sk = uint256S("1");
    uint256 zero = uint256S("0");
    uint256 sn_A = Compute_PRF(sk, zero);

    uint256 r_A = uint256S("123456");
    Note note = Note(0, sn_A, r_A);
    uint256 cmtA = note.cm();

    cout << "Trying to generate createAccount proof..." << endl;

    struct timeval gen_start, gen_end;
    struct timeval ver_start, ver_end;
    double genTimeUse;
    double verTimeUse;
    gettimeofday(&gen_start,NULL);

    auto proof = generate_createAccount_proof<default_r1cs_gg_ppzksnark_pp>(keypair.pk,
                                                                              sk,
                                                                              r_A,
                                                                              sn_A,
                                                                              cmtA
                                                                              );

    gettimeofday(&gen_end, NULL);
    genTimeUse = gen_end.tv_sec - gen_start.tv_sec + (gen_end.tv_usec - gen_start.tv_usec)/1000000.0;
    printf("\n\nGen CreateAccount Proof Use Time:%fs\n\n", genTimeUse);

    if (!proof) {
        printf("generate createAccount proof fail!!!\n");
        return false;
    } else {
        gettimeofday(&ver_start, NULL);
        bool result = verify_createAccount_proof(keypair.vk,
                                                  *proof,
                                                  cmtA
                                                  );

        if (!result){
            cout << "Verifying createAccount proof unsuccessfully!!!" << endl;
        } else {
            gettimeofday(&ver_end, NULL);
            verTimeUse = ver_end.tv_sec - ver_start.tv_sec + (ver_end.tv_usec - ver_start.tv_usec)/1000000.0;
            cout << "Verifying createAccount proof successfully!!!" << endl;
            printf("\n\nVerifying CreateAccount Proof Use Time:%fs\n\n", verTimeUse);
        }

        return result;
    }
}

template<typename ppzksnark_ppT>
r1cs_gg_ppzksnark_keypair<ppzksnark_ppT> Setup() {
    default_r1cs_gg_ppzksnark_pp::init_public_params();

    typedef libff::Fr<ppzksnark_ppT> FieldT;

    protoboard<FieldT> pb;

    createAccount_gadget<FieldT> createAccount(pb);
    createAccount.generate_r1cs_constraints();

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
    printf("\n\nCreateAccount Setup Time Usage:%fs\n\n",timeuse);

    libff::print_header("#             testing createAccount gadget");

    test_createAccount_gadget_with_instance<default_r1cs_gg_ppzksnark_pp>(keypair);

    return 0;
}
