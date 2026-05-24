#include "deps/sha256.h"
#include "uint256.h"
#include "util.h"

class Note {
public:
    uint64_t value;
    uint256 sn;
    uint256 r;

    Note(uint64_t value, uint256 sn, uint256 r)
        : value(value), sn(sn), r(r) {}

    uint256 cm() const{
        CSHA256 hasher;
        auto value_vec = convertIntToVectorLE(value);
        hasher.Write(&value_vec[0], value_vec.size());
        hasher.Write(sn.begin(), 32);
        hasher.Write(r.begin(), 32);
        uint256 result;
        hasher.Finalize(result.begin());
        return result;
    }
};
