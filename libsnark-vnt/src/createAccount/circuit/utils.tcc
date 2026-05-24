// Convert bool bits to a pb_variable_array
template<typename FieldT>
pb_variable_array<FieldT> from_bits(std::vector<bool> bits, pb_variable<FieldT>& ZERO) {
    pb_variable_array<FieldT> acc;

    BOOST_FOREACH(bool bit, bits) {
        acc.emplace_back(bit ? ONE : ZERO); // ONE is the constant 1, ZERO corresponds to FieldT::zero()
    }

    return acc;
}

// Take the trailing 252 bits out of a 256-bit input
std::vector<bool> trailing252(std::vector<bool> input) {
    if (input.size() != 256) {
        throw std::length_error("trailing252 input invalid length");
    }

    return std::vector<bool>(input.begin() + 4, input.end());
}

// Type conversion: uint256 -> bit array
std::vector<bool> uint256_to_bool_vector(uint256 input) {
    std::vector<unsigned char> input_v(input.begin(), input.end());
    std::vector<bool> output_bv(256, 0);
    convertBytesVectorToVector(
        input_v,
        output_bv
    );

    return output_bv;
}

// Type conversion: uint160 -> bit array
std::vector<bool> uint160_to_bool_vector(uint160 input) {
    std::vector<unsigned char> input_v(input.begin(), input.end());
    std::vector<bool> output_bv(160, 0);
    convertBytesVectorToVector(
        input_v,
        output_bv
    );

    return output_bv;
}

// Type conversion: uint64 -> bit array
std::vector<bool> uint64_to_bool_vector(uint64_t input) {
    auto num_bv = convertIntToVectorLE(input);
    std::vector<bool> num_v(64, 0);
    convertBytesVectorToVector(num_bv, num_v);

    return num_v;
}

// Append `from` to the end of the `into` array
void insert_uint256(std::vector<bool>& into, uint256 from) {
    std::vector<bool> blob = uint256_to_bool_vector(from);
    into.insert(into.end(), blob.begin(), blob.end());
}

// Append `from` to the end of the `into` array
void insert_uint160(std::vector<bool>& into, uint160 from) {
    std::vector<bool> blob = uint160_to_bool_vector(from);
    into.insert(into.end(), blob.begin(), blob.end());
}

// Append `from` to the end of the `into` array
void insert_uint64(std::vector<bool>& into, uint64_t from) {
    std::vector<bool> num = uint64_to_bool_vector(from);
    into.insert(into.end(), num.begin(), num.end());
}

// Swap endianness: with 32 as the axis of symmetry, reverse every 8 bits
template<typename T>
T swap_endianness_u64(T v) {
    if (v.size() != 64) {
        throw std::length_error("invalid bit length for 64-bit unsigned integer");
    }

    for (size_t i = 0; i < 4; i++) {
        for (size_t j = 0; j < 8; j++) {
            std::swap(v[i*8 + j], v[((7-i)*8)+j]);
        }
    }

    return v;
}

// Convert a bit array into its decimal value, still expressed as a linear combination
template<typename FieldT>
linear_combination<FieldT> packed_addition(pb_variable_array<FieldT> input) {
    auto input_swapped = swap_endianness_u64(input);

    return pb_packing_sum<FieldT>(pb_variable_array<FieldT>( 
        input_swapped.rbegin(), input_swapped.rend() // reverse_iterator giving reversed order
    ));
}

// Convert a bit array into its decimal value, expressed in the field form
template<typename FieldT>
FieldT packed_addition_fieldT(pb_variable_array<FieldT> input) {
    auto input_swapped = swap_endianness_u64(input);

    return pb_packing_filedT_sum<FieldT>(pb_variable_array<FieldT>( 
        input_swapped.rbegin(), input_swapped.rend() // reverse_iterator giving reversed order
    ));
}
