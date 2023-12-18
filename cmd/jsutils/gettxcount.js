import { ethers } from "ethers";
import program from "commander";

program.option("--rpc <rpc>", "Rpc");
program.option("--startNum <startNum>", "start num")
program.option("--endNum <endNum>", "end num")
program.parse(process.argv);

const provider = new ethers.JsonRpcProvider(program.rpc)

const main = async () => {
    let fromTo = 0
    let fromToLength = 0
    let fromToLengthBsc20 = 0
    console.log("Find the txs count between", program.startNum, "and", program.endNum);
    for (let i = program.startNum; i < program.endNum; i++) {
        let txs = await provider.send("eth_getTransactionsByBlockNumber", [ethers.toQuantity(i)]);
         for (let j = 0; j < txs.length; j++) {
             let tx = txs[j];
             if (tx.from === tx.to) {
                 fromTo++
                 if (tx.input.length > 40) {
                     fromToLength++
                     if (tx.input.includes("6d696e74")) {
                         fromToLengthBsc20++
                     }
                 }
             }
         }
    }
    console.log("fromTo =", fromTo, "fromToLength = ", fromToLength, "fromToLengthBsc20 = ", fromToLengthBsc20);
};

main().then(() => process.exit(0))
    .catch((error) => {
        console.error(error);
        process.exit(1);
    });