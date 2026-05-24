#include "utils.tcc"
#include "commitment.tcc"

/***********************************************************
 * createAccount circuit
 ***********************************************************
 * publicData: cmt_A
 * privateData: sk_A, r_A, sn_A
 ***********************************************************
 * Constraints:
 *   sn_A = SHA256(sk_A, 0)
 *   cmt_A = sha256_CMTA(0, sn_A, r_A)
 **********************************************************/
template<typename FieldT>
class createAccount_gadget : public gadget<FieldT> {
public:
    pb_variable_array<FieldT> zk_packed_inputs;
    pb_variable_array<FieldT> zk_unpacked_inputs;
    std::shared_ptr<multipacking_gadget<FieldT>> unpacker;

    std::shared_ptr<digest_variable<FieldT>> sk;
    std::shared_ptr<digest_variable<FieldT>> r_A;
    std::shared_ptr<digest_variable<FieldT>> sn_A;

    pb_variable_array<FieldT> value_zero;

    std::shared_ptr<digest_variable<FieldT>> cmtA;
    std::shared_ptr<sha256_PRF_gadget<FieldT>> prf_to_inputs_sn;
    std::shared_ptr<sha256_CMTA_gadget<FieldT>> commit_to_inputs_cmt;

    std::shared_ptr<digest_variable<FieldT>> zero_for_prf;

    pb_variable<FieldT> ZERO;

    createAccount_gadget(
        protoboard<FieldT>& pb
    ) : gadget<FieldT>(pb) {
        {
            zk_packed_inputs.allocate(pb, verifying_field_element_size());
            this->pb.set_input_sizes(verifying_field_element_size());

            alloc_uint256(zk_unpacked_inputs, cmtA);

            assert(zk_unpacked_inputs.size() == verifying_input_bit_size());

            unpacker.reset(new multipacking_gadget<FieldT>(
                pb, zk_unpacked_inputs, zk_packed_inputs,
                FieldT::capacity(), "unpacker"
            ));
        }

        ZERO.allocate(this->pb, FMT(this->annotation_prefix, "zero"));

        sk.reset(new digest_variable<FieldT>(pb, 256, "private key"));
        r_A.reset(new digest_variable<FieldT>(pb, 256, "random number"));
        sn_A.reset(new digest_variable<FieldT>(pb, 256, "serial number"));

        zero_for_prf.reset(new digest_variable<FieldT>(pb, 256, "zero_for_prf"));

        value_zero.allocate(pb, 64);

        prf_to_inputs_sn.reset(new sha256_PRF_gadget<FieldT>(
            pb, ZERO, sk->bits, zero_for_prf->bits, sn_A
        ));

        commit_to_inputs_cmt.reset(new sha256_CMTA_gadget<FieldT>(
            pb, ZERO, value_zero, sn_A->bits, r_A->bits, cmtA
        ));
    }

    void generate_r1cs_constraints() {
        unpacker->generate_r1cs_constraints(true);

        generate_r1cs_equals_const_constraint<FieldT>(this->pb, ZERO, FieldT::zero(), "ZERO");

        sk->generate_r1cs_constraints();
        r_A->generate_r1cs_constraints();
        sn_A->generate_r1cs_constraints();
        zero_for_prf->generate_r1cs_constraints();

        for (size_t i = 0; i < 64; i++) {
            generate_boolean_r1cs_constraint<FieldT>(this->pb, value_zero[i], "boolean_value_zero");
        }

        prf_to_inputs_sn->generate_r1cs_constraints();

        cmtA->generate_r1cs_constraints();
        commit_to_inputs_cmt->generate_r1cs_constraints();
    }

    void generate_r1cs_witness(
        uint256 sk_data,
        uint256 r_A_data,
        uint256 sn_A_data,
        uint256 cmtA_data
    ) {
        this->pb.val(ZERO) = FieldT::zero();

        sk->bits.fill_with_bits(this->pb, uint256_to_bool_vector(sk_data));
        r_A->bits.fill_with_bits(this->pb, uint256_to_bool_vector(r_A_data));
        sn_A->bits.fill_with_bits(this->pb, uint256_to_bool_vector(sn_A_data));
        zero_for_prf->bits.fill_with_bits(this->pb, std::vector<bool>(256, false));

        value_zero.fill_with_bits(this->pb, uint64_to_bool_vector(0));

        prf_to_inputs_sn->generate_r1cs_witness();
        commit_to_inputs_cmt->generate_r1cs_witness();

        cmtA->bits.fill_with_bits(this->pb, uint256_to_bool_vector(cmtA_data));

        unpacker->generate_r1cs_witness_from_bits();
    }

    static r1cs_primary_input<FieldT> witness_map(
        const uint256& cmtA
    ) {
        std::vector<bool> verify_inputs;
        insert_uint256(verify_inputs, cmtA);
        assert(verify_inputs.size() == verifying_input_bit_size());
        auto verify_field_elements = pack_bit_vector_into_field_element_vector<FieldT>(verify_inputs);
        assert(verify_field_elements.size() == verifying_field_element_size());
        return verify_field_elements;
    }

    static size_t verifying_input_bit_size() {
        size_t acc = 0;
        acc += 256; // cmtA
        return acc;
    }

    static size_t verifying_field_element_size() {
        return div_ceil(verifying_input_bit_size(), FieldT::capacity());
    }

    void alloc_uint256(pb_variable_array<FieldT>& packed_into,
                        std::shared_ptr<digest_variable<FieldT>>& var) {
        var.reset(new digest_variable<FieldT>(this->pb, 256, ""));
        packed_into.insert(packed_into.end(), var->bits.begin(), var->bits.end());
    }

    void alloc_uint64(pb_variable_array<FieldT>& packed_into,
                       pb_variable_array<FieldT>& integer) {
        integer.allocate(this->pb, 64, "");
        packed_into.insert(packed_into.end(), integer.begin(), integer.end());
    }
};
